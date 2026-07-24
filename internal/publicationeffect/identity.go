package publicationeffect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxCanonicalPathBytes = 4096
	maxRemoteURLBytes     = 4096
	maxRepositoryIDBytes  = 2048

	gitCommonDirIdentityPrefix = "git-common-dir:sha256:"
	gitDirIdentityPrefix       = "git-dir:sha256:"
	worktreeIdentityPrefix     = "worktree:sha256:"
)

type localPathIdentityMaterial struct {
	Version       int    `json:"version"`
	Domain        string `json:"domain"`
	CanonicalPath string `json:"canonicalPath"`
}

// GitCommonDirIdentityFromCanonicalPath derives a credential-free identity
// from a caller-resolved canonical git-common-dir path. It does not access the
// filesystem. Callers must resolve symlinks and platform-specific aliases
// before calling it.
func GitCommonDirIdentityFromCanonicalPath(path string) (string, error) {
	return localPathIdentity(
		path,
		"git_common_dir",
		gitCommonDirIdentityPrefix,
	)
}

// GitDirIdentityFromCanonicalPath derives an identity for one linked
// worktree's private Git directory. It distinguishes a worktree recreated at
// the same filesystem path under the same common repository.
func GitDirIdentityFromCanonicalPath(path string) (string, error) {
	return localPathIdentity(path, "git_dir", gitDirIdentityPrefix)
}

// WorktreeIdentityFromCanonicalPath derives a credential-free identity from a
// caller-resolved canonical worktree path. It does not access the filesystem.
func WorktreeIdentityFromCanonicalPath(path string) (string, error) {
	return localPathIdentity(path, "worktree", worktreeIdentityPrefix)
}

func localPathIdentity(path, domain, prefix string) (string, error) {
	if err := validateText(
		path,
		"canonical path",
		maxCanonicalPathBytes,
		controlNone,
	); err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("canonical path must be absolute")
	}
	if filepath.Clean(path) != path {
		return "", errors.New("canonical path must already be clean")
	}
	if filepath.Dir(path) == path {
		return "", errors.New("canonical path cannot be a filesystem root")
	}
	encoded, err := json.Marshal(localPathIdentityMaterial{
		Version:       SchemaVersion,
		Domain:        domain,
		CanonicalPath: path,
	})
	if err != nil {
		return "", fmt.Errorf("encode local path identity: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return prefix + hex.EncodeToString(sum[:]), nil
}

func validateLocalIdentity(value, prefix, field string) error {
	if err := validateText(value, field, len(prefix)+sha256.Size*2, controlNone); err != nil {
		return err
	}
	if !strings.HasPrefix(value, prefix) ||
		len(value) != len(prefix)+sha256.Size*2 {
		return fmt.Errorf("%s must be a canonical %s identity", field, strings.TrimSuffix(prefix, ":"))
	}
	digest := strings.TrimPrefix(value, prefix)
	if !validSHA256(digest) {
		return fmt.Errorf("%s must contain a lowercase SHA-256 digest", field)
	}
	return nil
}

type remoteTarget struct {
	Transport      string `json:"transport"`
	Host           string `json:"host"`
	Port           uint16 `json:"port,omitempty"`
	RepositoryPath string `json:"repositoryPath"`
}

func (r remoteTarget) repositoryIdentity() string {
	host := repositoryIdentityHost(r.Host, r.Port)
	return host + "/" + r.RepositoryPath
}

// RepositoryIdentityFromRemote derives the canonical credential-free
// repository identity stored in push and PR descriptors.
func RepositoryIdentityFromRemote(remoteURL string) (string, error) {
	target, err := parseRemoteTarget(remoteURL)
	if err != nil {
		return "", err
	}
	identity := target.repositoryIdentity()
	if err := validateRepositoryIdentity(identity); err != nil {
		return "", err
	}
	return identity, nil
}

func parseRemoteTarget(raw string) (remoteTarget, error) {
	if err := validateText(
		raw,
		"remote URL",
		maxRemoteURLBytes,
		controlNone,
	); err != nil {
		return remoteTarget{}, err
	}
	if strings.TrimSpace(raw) != raw {
		return remoteTarget{}, errors.New("remote URL cannot have surrounding whitespace")
	}
	if strings.Contains(raw, "\\") {
		return remoteTarget{}, errors.New("remote URL cannot contain backslashes")
	}

	if !strings.Contains(raw, "://") {
		return parseSCPRemote(raw)
	}
	rawScheme, _, _ := strings.Cut(raw, "://")
	if rawScheme != strings.ToLower(rawScheme) {
		return remoteTarget{}, errors.New("remote URL scheme must be lowercase")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return remoteTarget{}, fmt.Errorf("parse remote URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "ssh" {
		return remoteTarget{}, errors.New("remote URL must use https or ssh")
	}
	if parsed.Opaque != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.RawFragment != "" {
		return remoteTarget{}, errors.New(
			"remote URL cannot contain opaque, query, or fragment data",
		)
	}
	if parsed.RawPath != "" || strings.Contains(parsed.Path, "%") {
		return remoteTarget{}, errors.New("remote URL cannot contain escaped path data")
	}
	if err := validateRemoteUser(parsed, scheme); err != nil {
		return remoteTarget{}, err
	}
	host, port, err := canonicalRemoteHost(parsed.Hostname(), parsed.Port(), scheme)
	if err != nil {
		return remoteTarget{}, err
	}
	repositoryPath, err := canonicalRepositoryPath(parsed.Path)
	if err != nil {
		return remoteTarget{}, err
	}
	return remoteTarget{
		Transport:      scheme,
		Host:           host,
		Port:           port,
		RepositoryPath: repositoryPath,
	}, nil
}

func parseSCPRemote(raw string) (remoteTarget, error) {
	if strings.Count(raw, ":") != 1 {
		return remoteTarget{}, errors.New(
			"remote URL must be an explicit URL or one unambiguous scp target",
		)
	}
	hostPart, path, ok := strings.Cut(raw, ":")
	if !ok || hostPart == "" || path == "" ||
		strings.ContainsAny(hostPart, "/?#") ||
		strings.ContainsAny(path, "?#") {
		return remoteTarget{}, errors.New("remote scp target is invalid")
	}
	host := hostPart
	if user, rawHost, found := strings.Cut(hostPart, "@"); found {
		if user != "git" || rawHost == "" || strings.Contains(rawHost, "@") {
			return remoteTarget{}, errors.New(
				"remote URL cannot contain embedded credentials",
			)
		}
		host = rawHost
	}
	host, port, err := canonicalRemoteHost(host, "", "ssh")
	if err != nil {
		return remoteTarget{}, err
	}
	repositoryPath, err := canonicalRepositoryPath(path)
	if err != nil {
		return remoteTarget{}, err
	}
	return remoteTarget{
		Transport:      "ssh",
		Host:           host,
		Port:           port,
		RepositoryPath: repositoryPath,
	}, nil
}

func validateRemoteUser(parsed *url.URL, scheme string) error {
	if parsed.User == nil {
		return nil
	}
	password, hasPassword := parsed.User.Password()
	if hasPassword || password != "" {
		return errors.New("remote URL cannot contain embedded credentials")
	}
	username := parsed.User.Username()
	// "git" is the conventional public SSH account, not a credential. It is
	// accepted on input but never retained in the descriptor.
	if scheme != "ssh" || username != "git" {
		return errors.New("remote URL cannot contain embedded credentials")
	}
	return nil
}

func canonicalRemoteHost(rawHost, rawPort, scheme string) (string, uint16, error) {
	if rawHost == "" {
		return "", 0, errors.New("remote URL requires a host")
	}
	if err := validateText(rawHost, "remote host", 253, controlNone); err != nil {
		return "", 0, err
	}
	if strings.ToLower(rawHost) != rawHost {
		return "", 0, errors.New("remote host must be lowercase")
	}
	if strings.ContainsAny(rawHost, "@/%") {
		return "", 0, errors.New("remote host is invalid")
	}
	if ip := net.ParseIP(rawHost); ip == nil {
		if !validDNSHost(rawHost) {
			return "", 0, errors.New("remote host is invalid")
		}
	} else {
		rawHost = strings.ToLower(ip.String())
	}

	if rawPort == "" {
		return rawHost, 0, nil
	}
	portValue, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || portValue == 0 {
		return "", 0, errors.New("remote URL port must be between 1 and 65535")
	}
	port := uint16(portValue)
	if (scheme == "https" && port == 443) ||
		(scheme == "ssh" && port == 22) {
		return "", 0, errors.New("remote URL must omit its default port")
	}
	return rawHost, port, nil
}

func validDNSHost(host string) bool {
	if len(host) == 0 || len(host) > 253 ||
		strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, value := range label {
			if (value < 'a' || value > 'z') &&
				(value < '0' || value > '9') && value != '-' {
				return false
			}
		}
	}
	return true
}

func canonicalRepositoryPath(raw string) (string, error) {
	path := strings.TrimPrefix(raw, "/")
	if path == "" || strings.HasPrefix(path, "/") ||
		strings.HasSuffix(path, "/") {
		return "", errors.New("remote URL requires a canonical repository path")
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", errors.New(
			"remote repository path must include an owner and repository",
		)
	}
	for _, part := range parts {
		if err := validateText(part, "remote repository path component", 255, controlNone); err != nil {
			return "", err
		}
		if part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return "", errors.New("remote repository path is not canonical")
		}
		if strings.ContainsAny(part, ":@%") ||
			strings.IndexFunc(part, func(value rune) bool {
				return value == unicode.ReplacementChar ||
					unicode.IsSpace(value)
			}) >= 0 {
			return "", errors.New("remote repository path is invalid")
		}
	}
	last := len(parts) - 1
	if strings.HasSuffix(parts[last], ".git") {
		parts[last] = strings.TrimSuffix(parts[last], ".git")
		if parts[last] == "" || strings.HasSuffix(parts[last], ".git") {
			return "", errors.New("remote repository path is not canonical")
		}
	}
	path = strings.Join(parts, "/")
	if len(path) > maxRepositoryIDBytes {
		return "", fmt.Errorf(
			"remote repository path must be at most %d bytes",
			maxRepositoryIDBytes,
		)
	}
	return path, nil
}

func validateRepositoryIdentity(identity string) error {
	if err := validateText(
		identity,
		"repository identity",
		maxRepositoryIDBytes,
		controlNone,
	); err != nil {
		return err
	}
	if strings.Contains(identity, "://") || strings.ContainsAny(identity, "@?#\\") {
		return errors.New("repository identity must be credential-free")
	}
	hostPart, path, ok := strings.Cut(identity, "/")
	if !ok || hostPart == "" || path == "" {
		return errors.New(
			"repository identity must be host/owner/repository",
		)
	}
	parsedHost, err := url.Parse("https://" + hostPart)
	if err != nil || parsedHost.Host == "" || parsedHost.Path != "" {
		return errors.New("repository identity host is invalid")
	}
	host, port, err := canonicalRemoteHost(
		parsedHost.Hostname(),
		parsedHost.Port(),
		"identity",
	)
	if err != nil {
		return fmt.Errorf("repository identity: %w", err)
	}
	canonicalPath, err := canonicalRepositoryPath(path)
	if err != nil {
		return fmt.Errorf("repository identity: %w", err)
	}
	canonicalHost := repositoryIdentityHost(host, port)
	if identity != canonicalHost+"/"+canonicalPath {
		return errors.New("repository identity must already be canonical")
	}
	return nil
}

func validatePRRepositoryIdentity(identity string) error {
	if err := validateRepositoryIdentity(identity); err != nil {
		return err
	}
	if identity != strings.ToLower(identity) {
		return errors.New(
			"pull request repository identity must use lowercase characters",
		)
	}
	_, path, _ := strings.Cut(identity, "/")
	if len(strings.Split(path, "/")) != 2 {
		return errors.New(
			"pull request repository identity must be host/owner/repository",
		)
	}
	return nil
}

func repositoryIdentityHost(host string, port uint16) string {
	if port != 0 {
		return net.JoinHostPort(host, strconv.Itoa(int(port)))
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}
