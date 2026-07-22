# TaskCircuit 설치 및 업그레이드

TaskCircuit은 Web UI와 SQLite 엔진을 포함한 단일 실행 파일이다. 사용하는 컴퓨터에는 Node.js, npm, Bun, Go, 별도 데이터베이스 서버가 필요하지 않다. Claude Code, Codex, 수정된 Cline, Gemini CLI는 실제 worker 또는 planner로 선택한 것만 설치하면 된다.

## 1. 릴리스 바이너리 설치

[GitHub Releases](https://github.com/nn1a/kanban/releases)에서 운영체제와 CPU에 맞는 파일 및 `checksums.txt`를 내려받는다.

| 환경 | 릴리스 이름 |
| --- | --- |
| 일반 Linux x86-64 | `taskcircuit_<version>_linux_amd64.tar.gz` |
| 일반 Linux ARM64 | `taskcircuit_<version>_linux_arm64.tar.gz` |
| Alpine/musl x86-64 | `taskcircuit_<version>_linux_musl_amd64.tar.gz` |
| Alpine/musl ARM64 | `taskcircuit_<version>_linux_musl_arm64.tar.gz` |
| macOS Intel | `taskcircuit_<version>_darwin_amd64.tar.gz` |
| macOS Apple Silicon | `taskcircuit_<version>_darwin_arm64.tar.gz` |
| Windows x86-64 | `taskcircuit_<version>_windows_amd64.tar.gz` |
| Windows ARM64 | `taskcircuit_<version>_windows_arm64.tar.gz` |

Linux 바이너리는 `CGO_ENABLED=0`으로 만들어진 정적 실행 파일이다. glibc나 musl에 동적으로 연결되지 않으므로 별도 C 런타임 패키지가 필요 없다. `linux_musl_*`은 Alpine 사용자가 산출물을 명확히 선택할 수 있도록 구분한 이름이다.

Linux에서 체크섬을 검증하고 설치한다.

```bash
grep 'taskcircuit_<version>_<platform>_<architecture>.tar.gz' checksums.txt | sha256sum -c -
tar -xzf taskcircuit_<version>_<platform>_<architecture>.tar.gz
sudo install -m 0755 \
  taskcircuit_<version>_<platform>_<architecture>/taskcircuit \
  /usr/local/bin/taskcircuit
taskcircuit version
```

macOS에서는 같은 `checksums.txt`를 다음과 같이 검증한다.

```bash
grep 'taskcircuit_<version>_<platform>_<architecture>.tar.gz' checksums.txt | shasum -a 256 -c -
```

macOS에서 직접 내려받은 파일이 격리되었다면, 체크섬과 출처를 확인한 뒤 실행 파일의 quarantine 속성만 제거한다.

```bash
xattr -d com.apple.quarantine /usr/local/bin/taskcircuit
```

Windows PowerShell에서는 기본 제공 `tar`로 압축을 풀고, `taskcircuit.exe`가 있는 디렉터리를 사용자 `PATH`에 추가한다.

```powershell
Get-FileHash .\taskcircuit_<version>_windows_amd64.tar.gz -Algorithm SHA256
tar -xzf taskcircuit_<version>_windows_amd64.tar.gz
& ".\taskcircuit_<version>_windows_amd64\taskcircuit.exe" version
```

출력된 SHA-256 값을 `checksums.txt`의 같은 파일 행과 대조한다.

## 2. 최초 실행

데이터를 둘 프로젝트 디렉터리에서 초기화하고 대시보드를 연다.

```bash
cd /path/to/project
taskcircuit init
taskcircuit dashboard
```

대시보드 명령이 출력한 bootstrap URL을 브라우저에서 한 번 연다. URL 토큰은 HTTP-only 세션 쿠키로 교환되고 깨끗한 URL로 리다이렉트된다. 기본 주소는 `127.0.0.1:8420`이며 Web UI 파일은 바이너리에 포함되어 있다.

기본 데이터 위치는 실행한 디렉터리를 기준으로 다음과 같다.

```text
data/
├─ kanban.db
├─ attachments/
├─ logs/
├─ workspaces/
└─ boards/<board-slug>/
```

서비스나 에디터에서 작업 디렉터리가 달라질 수 있으면 `--db`에 절대 경로를 지정한다.

```bash
taskcircuit dashboard --db /absolute/path/to/data/kanban.db
taskcircuit serve --db /absolute/path/to/data/kanban.db
```

## 3. Claude Code와 Codex에 MCP 연결

MCP 클라이언트에는 `taskcircuit`의 절대 경로를 등록하는 편이 안전하다.

```bash
TASKCIRCUIT_BIN=$(command -v taskcircuit)
TASKCIRCUIT_DB="$PWD/data/kanban.db"

claude mcp add --scope local taskcircuit -- \
  "$TASKCIRCUIT_BIN" serve --db "$TASKCIRCUIT_DB"

codex mcp add taskcircuit -- \
  "$TASKCIRCUIT_BIN" serve --db "$TASKCIRCUIT_DB"
```

설정 파일을 직접 관리한다면 [Claude 예제](../examples/claude.mcp.json)와 [Codex 예제](../examples/codex.config.toml)의 절대 경로만 설치 위치에 맞게 바꾼다.

## 4. MCP가 비활성화된 Cline 연결

Cline 쪽 MCP 기능은 필요하지 않다. 수정된 Cline이 다음 계약을 만족하면 TaskCircuit dispatcher가 CLI로 상태를 전달한다.

- `--json`, `--cwd <path>`, `--auto-approve <boolean>`을 받는다.
- 마지막 위치 인자로 worker prompt를 받는다.
- `TASKCIRCUIT_*` 환경을 상속하는 shell 도구를 제공한다.
- stdout에 NDJSON을 출력하고 정상 turn에서 종료 코드 0을 반환한다.

실행 파일 이름이 `cline`이 아니면 경로를 지정한다.

```bash
export TASKCIRCUIT_CLINE_BIN=/absolute/path/to/modified-cline

taskcircuit create "수정된 Cline CLI 브리지 검증" \
  --assignee cline-worker \
  --runtime cline \
  --workspace "$PWD"
taskcircuit dispatch --once
```

dispatcher는 claim된 task, run, token과 정확히 일치하는 `taskcircuit heartbeat`, `comment`, `complete`, `block` 명령을 prompt에 넣는다. 다른 task를 수정하는 lifecycle 명령은 거부된다. 자세한 계약은 [Cline CLI 브리지 문서](../examples/cline-cli-bridge.md)를 참고한다.

Cline을 보조 planner로도 사용할 수 있다.

```bash
taskcircuit specify <triage-task-id> --planner-runtime cline
taskcircuit decompose <triage-task-id> \
  --planner-runtime cline \
  --profile "worker:cline:범위가 지정된 작업을 구현하고 검증한다"
```

planner 실행은 도구를 쓰지 않는 읽기 전용 구조화 출력 단계다. Cline의 최종 NDJSON 결과가 스키마를 통과한 뒤에만 보드가 변경된다.

## 5. 소스에서 빌드

릴리스 바이너리를 사용하는 일반 사용자에게는 Go가 필요하지 않다. 개발자만 Go 1.25 이상에서 다음 명령을 사용한다. race 검증에는 해당 플랫폼의 C 컴파일러가 추가로 필요하다.

```bash
make build
./bin/taskcircuit version
make verify
```

Go 도구가 이미 설치되어 있다면 직접 설치할 수도 있다.

```bash
go install github.com/nn1a/kanban/cmd/taskcircuit@latest
```

관리자가 모든 플랫폼용 릴리스 파일을 만들 때는 비어 있는 `release/` 디렉터리에서 다음 명령을 실행한다. GitHub Actions 설정은 필요하지 않다.

```bash
make release VERSION=v1.0.0
```

## 6. 업그레이드와 백업

1. 실행 중인 `dashboard`와 `dispatch --watch` 프로세스를 정상 종료한다.
2. `data/kanban.db`, `data/boards/`, `data/attachments/`를 백업한다.
3. 새 아카이브의 체크섬을 검증한다.
4. 기존 실행 파일만 새 바이너리로 교체한다.
5. `taskcircuit version`, `taskcircuit diagnostics`를 실행하고 대시보드를 확인한다.

데이터와 Web UI는 실행 파일과 분리되어 있다. 새 바이너리가 데이터베이스를 열 때 필요한 스키마 마이그레이션을 수행한다. 여러 버전의 dispatcher나 dashboard가 같은 데이터베이스를 동시에 열지 않도록 프로세스를 모두 내린 뒤 교체한다.

문제가 있으면 기존 바이너리와 백업한 데이터 디렉터리로 함께 되돌린다. 실행 파일만 낮은 버전으로 바꾸고 새 스키마 데이터베이스를 그대로 여는 방식은 권장하지 않는다.
