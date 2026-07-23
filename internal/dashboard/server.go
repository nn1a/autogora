package dashboard

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nn1a/autogora/internal/agentconfig"
	"github.com/nn1a/autogora/internal/boards"
	"github.com/nn1a/autogora/internal/supervisor"
	webui "github.com/nn1a/autogora/web"
)

type Options struct {
	DBPath         string
	CLIPath        string
	Host           string
	Port           int
	Token          string
	OnLog          func(string)
	AgentDetection agentconfig.DetectOptions
}

type Server struct {
	HTTP       *http.Server
	Listener   net.Listener
	Token      string
	URL        string
	manager    *boards.Manager
	options    Options
	ctx        context.Context
	cancel     context.CancelFunc
	workers    sync.WaitGroup
	supervisor *supervisor.Controller
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func Start(ctx context.Context, options Options) (*Server, error) {
	manager, err := boards.NewManager(options.DBPath)
	if err != nil {
		return nil, err
	}
	if _, err := manager.Create(ctx, "default", boards.Update{}); err != nil {
		return nil, err
	}
	token := options.Token
	if token == "" {
		token, err = randomToken()
		if err != nil {
			return nil, err
		}
	}
	if len(token) < 16 {
		return nil, errors.New("dashboard token must contain at least 16 characters")
	}
	if options.Host == "" {
		options.Host = "127.0.0.1"
	}
	serverContext, cancelServer := context.WithCancel(ctx)
	service := &Server{Token: token, manager: manager, options: options, ctx: serverContext, cancel: cancelServer}
	service.supervisor = supervisor.New(supervisor.Options{DBPath: options.DBPath, CLIPath: options.CLIPath, OnLog: options.OnLog})
	service.HTTP = &http.Server{Handler: service, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	listener, err := net.Listen("tcp", net.JoinHostPort(options.Host, strconv.Itoa(options.Port)))
	if err != nil {
		return nil, err
	}
	service.Listener = listener
	host := listener.Addr().String()
	service.URL = "http://" + host
	if config, configErr := agentconfig.Load(agentconfig.Options{}); configErr != nil {
		if options.OnLog != nil {
			options.OnLog("global agent configuration was not loaded: " + configErr.Error())
		}
	} else if config.Supervisor.AutoStart {
		service.supervisor.Start(serverContext, config)
	}
	go func() {
		if err := service.HTTP.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && options.OnLog != nil {
			options.OnLog("dashboard server failed: " + err.Error())
		}
	}()
	go func() {
		<-serverContext.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = service.HTTP.Shutdown(shutdown)
	}()
	return service, nil
}

func (s *Server) Close(ctx context.Context) error {
	s.cancel()
	supervisorErr := s.supervisor.Stop(ctx)
	err := s.HTTP.Shutdown(ctx)
	s.workers.Wait()
	return errors.Join(supervisorErr, err)
}

func securityHeaders(response http.ResponseWriter) {
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("X-Frame-Options", "DENY")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
}

func secureEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func requestToken(request *http.Request) string {
	if authorization := request.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
		return strings.TrimPrefix(authorization, "Bearer ")
	}
	if cookie, err := request.Cookie("autogora_session"); err == nil {
		return cookie.Value
	}
	return request.URL.Query().Get("token")
}

func sendJSON(response http.ResponseWriter, status int, value any) {
	securityHeaders(response)
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}

func errorStatus(err error) int {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "exceeds") && strings.Contains(message, "bytes"):
		return http.StatusRequestEntityTooLarge
	case strings.Contains(message, "not found"):
		return http.StatusNotFound
	case strings.Contains(message, "requires"), strings.Contains(message, "invalid"), strings.Contains(message, "must"), strings.Contains(message, "cannot be empty"):
		return http.StatusBadRequest
	case strings.Contains(message, "already"), strings.Contains(message, "cannot"), strings.Contains(message, "only"), strings.Contains(message, "cycle"), strings.Contains(message, "active"), strings.Contains(message, "terminal"):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func sendError(response http.ResponseWriter, err error) {
	sendJSON(response, errorStatus(err), map[string]any{"error": err.Error()})
}

func readBody(request *http.Request, limit int64) ([]byte, error) {
	defer request.Body.Close()
	value, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > limit {
		return nil, fmt.Errorf("request body exceeds %d bytes", limit)
	}
	return value, nil
}

func readJSON(request *http.Request) (map[string]any, error) {
	body, err := readBody(request, 1024*1024)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	value := map[string]any{}
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return value, nil
}

func decodeSegments(path string) ([]string, error) {
	result := []string{}
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		if segment == "" {
			continue
		}
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return nil, err
		}
		result = append(result, decoded)
	}
	return result, nil
}

func (s *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	securityHeaders(response)
	if request.URL.Path == "/" && request.URL.Query().Has("token") {
		if !secureEqual(request.URL.Query().Get("token"), s.Token) {
			sendJSON(response, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
			return
		}
		http.SetCookie(response, &http.Cookie{Name: "autogora_session", Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		response.Header().Set("Cache-Control", "no-store")
		http.Redirect(response, request, "/", http.StatusFound)
		return
	}
	if !secureEqual(requestToken(request), s.Token) {
		sendJSON(response, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
		return
	}
	if !strings.HasPrefix(request.URL.Path, "/api/") {
		s.serveStatic(response, request)
		return
	}
	segments, err := decodeSegments(request.URL.Path)
	if err != nil {
		sendError(response, err)
		return
	}
	if err := s.handleAPI(response, request, segments); err != nil {
		sendError(response, err)
	}
}

func (s *Server) serveStatic(response http.ResponseWriter, request *http.Request) {
	files := map[string]string{"/": "index.html", "/app.js": "app.js", "/styles.css": "styles.css"}
	name, ok := files[request.URL.Path]
	if !ok {
		sendJSON(response, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	contents, err := fs.ReadFile(webui.Files, name)
	if err != nil {
		sendError(response, err)
		return
	}
	contentTypes := map[string]string{"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "styles.css": "text/css; charset=utf-8"}
	response.Header().Set("Content-Type", contentTypes[name])
	if name == "index.html" {
		response.Header().Set("Cache-Control", "no-store")
	} else {
		response.Header().Set("Cache-Control", "public, max-age=300")
	}
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(contents)
}
