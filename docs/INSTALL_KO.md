# Autogora 설치 및 업그레이드

Autogora는 Web UI와 SQLite 엔진을 포함한 단일 실행 파일이다. worker나 planner로 사용할 클라이언트만 설치하면 된다. Node.js, npm, Bun, Go, 별도 데이터베이스 서버는 필요하지 않다.

## 1. 릴리스 바이너리 설치

[GitHub Releases](https://github.com/nn1a/autogora/releases)에서 운영체제와 CPU에 맞는 파일 및 `checksums.txt`를 내려받는다.

| 환경 | 릴리스 이름 |
| --- | --- |
| 일반 Linux x86-64 | `autogora_<version>_linux_amd64.tar.gz` |
| 일반 Linux ARM64 | `autogora_<version>_linux_arm64.tar.gz` |
| Alpine/musl x86-64 | `autogora_<version>_linux_musl_amd64.tar.gz` |
| Alpine/musl ARM64 | `autogora_<version>_linux_musl_arm64.tar.gz` |
| macOS Intel | `autogora_<version>_darwin_amd64.tar.gz` |
| macOS Apple Silicon | `autogora_<version>_darwin_arm64.tar.gz` |
| Windows x86-64 | `autogora_<version>_windows_amd64.tar.gz` |
| Windows ARM64 | `autogora_<version>_windows_arm64.tar.gz` |

Linux 바이너리는 `CGO_ENABLED=0`인 정적 실행 파일이다. glibc나 musl에 동적으로 연결하지 않으므로 별도 C 런타임 패키지가 필요 없다. Alpine에서는 `linux_musl_*` 산출물을 선택한다.

Linux에서 체크섬을 검증하고 설치한다.

```bash
grep 'autogora_<version>_<platform>_<architecture>.tar.gz' checksums.txt | sha256sum -c -
tar -xzf autogora_<version>_<platform>_<architecture>.tar.gz
sudo install -m 0755 \
  autogora_<version>_<platform>_<architecture>/autogora \
  /usr/local/bin/autogora
autogora version
```

macOS에서는 같은 `checksums.txt`를 다음과 같이 검증한다.

```bash
grep 'autogora_<version>_<platform>_<architecture>.tar.gz' checksums.txt | shasum -a 256 -c -
```

macOS가 내려받은 파일을 격리했다면 체크섬과 출처를 확인한 뒤 실행 파일의 quarantine 속성을 제거한다.

```bash
xattr -d com.apple.quarantine /usr/local/bin/autogora
```

Windows PowerShell에서는 기본 제공 `tar`로 압축을 풀고, `autogora.exe`가 있는 디렉터리를 사용자 `PATH`에 추가한다.

```powershell
Get-FileHash .\autogora_<version>_windows_amd64.tar.gz -Algorithm SHA256
tar -xzf autogora_<version>_windows_amd64.tar.gz
& ".\autogora_<version>_windows_amd64\autogora.exe" version
```

명령이 출력한 SHA-256 값을 `checksums.txt`의 같은 파일 행과 대조한다.

## 2. 최초 실행

데이터를 둘 프로젝트 디렉터리에서 초기화하고 대시보드를 연다.

```bash
cd /path/to/project
autogora init
autogora dashboard
```

대시보드 명령이 출력한 bootstrap URL을 브라우저에서 한 번 연다. 브라우저는 URL 토큰을 HTTP-only 세션 쿠키로 교환한 뒤 토큰 없는 URL로 이동한다. 기본 주소는 `127.0.0.1:8420`이며 Web UI 파일은 바이너리에 들어 있다.

기본 데이터는 Git 작업 트리 밖의 운영체제별 사용자 데이터 디렉터리에 저장된다. 한 clone의 worktree들은 상태를 공유하고 다른 clone은 분리된다. 프로젝트의 어느 하위 디렉터리에서든 실제 경로를 확인할 수 있으며, `paths`는 디렉터리나 DB를 만들지 않는다.

```bash
autogora paths
```

| 운영체제 | 기본 Autogora 데이터 루트 |
| --- | --- |
| Linux | `$XDG_DATA_HOME/autogora` 또는 `~/.local/share/autogora` |
| macOS | `~/Library/Application Support/autogora` |
| Windows | `%LOCALAPPDATA%\autogora` |

프로젝트별 경로 내부는 다음 구조를 사용한다.

```text
<app-data-root>/projects/<project-name>-<hash>/
├─ autogora.db
├─ attachments/
├─ logs/
├─ workspaces/
└─ boards/<board-slug>/
```

전체 Autogora 데이터 루트를 다른 디스크로 옮기려면 절대 경로의 `AUTOGORA_DATA_HOME`을 설정한다. 특정 명령만 다른 DB로 연결하려면 `--db` 또는 `AUTOGORA_DB`를 사용한다. 우선순위는 `--db`, `AUTOGORA_DB`, 저장된 프로젝트별 위치, 운영체제 기본 위치 순서다.

```bash
export AUTOGORA_DATA_HOME=/absolute/path/to/autogora-data
autogora dashboard --db /absolute/path/to/autogora.db
autogora serve --db /absolute/path/to/autogora.db
```

데이터가 반드시 저장소 디렉터리 안에 있어야 한다면 일반적인 `data/`나 `.git/` 내부 대신 프로젝트 루트의 `.autogora/`를 사용한다.

```bash
autogora init --data-dir .autogora
autogora paths
```

`.autogora/.gitignore`의 `*` 규칙은 SQLite DB와 WAL, 로그, 첨부파일, 작업 공간을 Git에서 제외한다. `.git` 내부에는 저장할 수 없다. 기본 위치로 돌아가려면 다음 명령을 사용한다.

```bash
autogora init --reset-data-dir
```

위치를 바꿔도 기존 데이터는 이동하거나 삭제되지 않으며 새 위치에 기본 보드가 생성된다. 기존 상태를 이어서 사용하려면 dispatcher와 dashboard를 종료하고 전체 데이터 루트를 복사한다.

저장소를 이동하면 Git common directory가 바뀌어 새 프로젝트 ID와 빈 기본 위치가 선택된다. 기존 데이터는 그대로 남는다. 이전 `dataRoot`를 계속 사용하려면 이동한 저장소에서 다시 연결한다.

```bash
autogora init --data-dir /absolute/previous/dataRoot
```

## 3. 권장 자동 설정: Skill 설치와 MCP 등록

Autogora 바이너리는 `autogora-worker`, `autogora-orchestrator` Skill을 내장한다. 별도 저장소나 npm 패키지 없이 프로젝트 디렉터리에서 Skill과 MCP를 함께 설정한다. `--dry-run`으로 변경 내용을 확인한 뒤 적용한다.

```bash
cd /path/to/project
autogora setup --client codex --dry-run
autogora setup --client codex
```

`--client`는 `codex`, `claude`, `gemini`, `all`을 받으며 여러 번 지정할 수 있다.

```bash
autogora setup --client claude --client codex --dry-run
autogora setup --client all
```

| 대상 | Skill 기본 위치 | MCP 기본 범위 |
| --- | --- | --- |
| Codex | 프로젝트 `.agents/skills/` | user |
| Claude Code | 프로젝트 `.claude/skills/` | local |
| Gemini CLI | 프로젝트 `.agents/skills/` | project |

Codex와 Gemini를 함께 선택하면 같은 `.agents/skills/` 대상은 한 번만 설치한다. 프로젝트 루트는 현재 위치에서 가장 가까운 `.git` 디렉터리를 기준으로 찾으며, `--project-dir`로 시작 위치를 명시할 수 있다.

필요하면 Skill과 MCP를 따로 관리한다.

```bash
# 내장 Skill만 설치·확인·제거
autogora skills install --client codex
autogora skills status --client codex
autogora skills uninstall --client codex

# MCP만 등록·확인·해제
autogora mcp register --client codex --dry-run
autogora mcp register --client codex
autogora mcp status --client codex
autogora mcp unregister --client codex
```

사용자 전체에서 Skill을 공유하려면 `skills` 명령에 `--scope user`를 사용한다. 통합 설정에서는 Skill과 MCP 범위를 독립적으로 지정할 수 있다.

```bash
autogora setup --client claude \
  --skill-scope user \
  --mcp-scope project
```

안전 규칙은 다음과 같다.

- 각 Skill에는 manifest와 SHA-256이 저장된다. 수정했거나 Autogora가 관리하지 않는 파일은 자동으로 덮어쓰거나 지우지 않는다. 내용을 확인한 뒤에만 `--force`를 사용한다.
- 같은 이름의 MCP 등록이 다른 바이너리 또는 데이터베이스를 가리키면 중단한다. 기존 등록을 확인한 뒤 교체할 때만 `--replace`를 사용한다.
- `setup`은 Skill과 MCP 양쪽을 먼저 점검한다. 클라이언트 실행 파일 누락이나 충돌을 발견하면 아무것도 적용하지 않는다.
- MCP 등록은 Autogora 바이너리와 데이터베이스의 절대 경로를 저장한다. 바이너리를 다른 경로로 옮기거나 데이터베이스 경로를 바꾸면 `mcp status`로 확인하고 `mcp register --replace`로 갱신한다.
- 명령별 옵션은 `autogora help setup`, `autogora help skills`, `autogora help mcp`에서 확인한다.

Codex CLI의 네이티브 등록은 user 범위를 사용한다. `setup --scope project --client codex`처럼 하나의 project 범위를 양쪽에 강제하지 말고, 기본값을 사용하거나 `--skill-scope project --mcp-scope user`로 분리한다.

## 4. 수동 MCP 연결

자동 설정을 사용할 수 없거나 설정 파일을 직접 관리할 때만 아래 절차를 사용한다. MCP 클라이언트에는 `autogora`의 절대 경로를 등록한다.

```bash
AUTOGORA_BIN=$(command -v autogora)
autogora paths  # 출력의 database 절대 경로를 아래에 사용한다.
AUTOGORA_DB=/absolute/path/printed/by/autogora/paths

claude mcp add --scope local autogora -- \
  "$AUTOGORA_BIN" serve --db "$AUTOGORA_DB"

codex mcp add autogora -- \
  "$AUTOGORA_BIN" serve --db "$AUTOGORA_DB"

gemini mcp add --scope project autogora "$AUTOGORA_BIN" serve -- \
  --db "$AUTOGORA_DB"
```

설정 파일을 직접 관리한다면 [Claude 예제](../examples/claude.mcp.json)와 [Codex 예제](../examples/codex.config.toml)의 절대 경로만 설치 위치에 맞게 바꾼다.

## 5. MCP가 비활성화된 Cline 연결

수정된 Cline이 다음 계약을 만족하면 Autogora dispatcher가 MCP 없이 CLI로 상태를 전달한다.

`autogora setup`, `skills`, `mcp`의 client 대상에는 Cline을 넣지 않는다. Cline은 전역 MCP/Skill 설치 대신 dispatcher가 실행마다 발급하는 task ID, run ID, claim token으로 CLI 브리지를 호출한다. 각 worker는 자신의 task만 완료하거나 수정할 수 있으며 MCP 기능이 없는 수정 버전에서도 동작한다.

- `--json`, `--cwd <path>`, `--auto-approve <boolean>`을 받는다.
- 마지막 위치 인자로 worker prompt를 받는다.
- `AUTOGORA_*` 환경을 상속하는 shell 도구를 제공한다.
- stdout에 NDJSON을 출력하고 정상 turn에서 종료 코드 0을 반환한다.

실행 파일 이름이 `cline`이 아니면 경로를 지정한다.

```bash
export AUTOGORA_CLINE_BIN=/absolute/path/to/modified-cline

autogora create "수정된 Cline CLI 브리지 검증" \
  --assignee cline-worker \
  --runtime cline \
  --workspace "$PWD"
autogora dispatch --once
```

dispatcher는 claim한 task, run, token과 일치하는 `autogora heartbeat`, `comment`, `complete`, `block` 명령을 prompt에 넣는다. 다른 task를 수정하는 lifecycle 명령은 거부된다. 전체 계약은 [Cline CLI 브리지 문서](../examples/cline-cli-bridge.md)에 있다.

Cline을 보조 planner로도 사용할 수 있다.

```bash
autogora specify <triage-task-id> --planner-runtime cline
autogora decompose <triage-task-id> \
  --planner-runtime cline \
  --profile "worker:cline:범위가 지정된 작업을 구현하고 검증한다"
```

planner는 도구 없이 읽기 전용 구조화 결과를 출력한다. Cline의 최종 NDJSON 결과가 스키마를 통과해야 보드에 반영된다.

## 6. 소스에서 빌드

릴리스 바이너리를 사용하는 데 Go는 필요하지 않다. 소스 빌드에는 Go 1.25 이상을 사용한다. race 검증에는 해당 플랫폼의 C 컴파일러도 필요하다.

```bash
make build
./bin/autogora version
make verify
```

Go 환경에서는 다음 명령으로 설치할 수도 있다.

```bash
go install -trimpath -ldflags='-s -w -buildid=' \
  github.com/nn1a/autogora/cmd/autogora@latest
```

모든 플랫폼용 릴리스 파일은 다음 명령으로 만든다. 출력할 `release/` 디렉터리는 비어 있어야 한다.

```bash
make release VERSION=v1.0.0
```

릴리스 스크립트는 경로·VCS 정보, 디버그·심볼 테이블, Go build ID를 제거하고 gzip 헤더의 원본 이름과 시간 정보도 남기지 않는다. 실행 파일이 기본 제한인 16MiB를 넘으면 빌드를 중단한다. 한도를 의도적으로 바꿀 때만 `MAX_BINARY_BYTES`를 지정한다.

```bash
MAX_BINARY_BYTES=18874368 make release VERSION=v1.0.0
```

## 7. 업그레이드와 백업

1. 실행 중인 `dashboard`와 `dispatch --watch` 프로세스를 정상 종료한다.
2. `autogora paths`로 확인한 `dataRoot` 전체를 백업한다.
3. 새 아카이브의 체크섬을 검증한다.
4. 기존 실행 파일만 새 바이너리로 교체한다.
5. `autogora version`, `autogora diagnostics`를 실행하고 대시보드를 확인한다.

데이터와 Web UI는 실행 파일과 분리된다. 새 바이너리는 데이터베이스를 열 때 필요한 스키마 마이그레이션을 수행한다. 여러 버전의 dispatcher나 dashboard가 같은 데이터베이스를 동시에 열지 않도록 관련 프로세스를 모두 종료한 뒤 교체한다.

문제가 있으면 기존 바이너리와 백업한 데이터 디렉터리로 함께 되돌린다. 실행 파일만 낮은 버전으로 바꾸고 새 스키마 데이터베이스를 그대로 여는 방식은 권장하지 않는다.
