# Autogora 실전 워크플로 가이드

아이디어를 `Triage`에 등록하고 작업을 구체화한 뒤 에이전트에게 맡기고, 검증 근거와 함께 `Done`으로 마무리한다. TUI, Web UI, CLI, MCP는 모두 같은 SQLite 상태와 전이 규칙을 공유한다.

예제의 `<task-id>`와 `<board>`는 실제 출력값으로 바꿔야 한다. `claude`, `codex`, `cline`, `gemini` 중 설치된 런타임만 사용한다.

## 1. 가장 짧은 성공 경로

```text
Triage ── specify ──> Todo ── promote/조건 충족 ──> Ready
                                                         │
                                                   dispatch/claim
                                                         │
                                                         v
                                                      Running
                                                         │
                                                complete + 검증 근거
                                                         │
                                                         v
                                                       Done
```

1. 불명확한 요청은 `Triage`에 등록한다.
2. 한 작업이면 `Specify`, 여러 독립 작업이면 `Decompose`를 사용한다.
3. 담당자, 런타임, 작업 디렉터리, 완료 조건을 확인하고 `Ready`로 보낸다.
4. dispatcher가 `Ready` 작업을 claim하여 `Running`으로 만들고 에이전트를 실행한다.
5. 에이전트가 테스트·검증 근거를 남기고 작업을 `Done`으로 마친다.

## 2. 최초 실행

```bash
autogora version
autogora init
autogora setup --client codex --dry-run
autogora setup --client codex
autogora tui
```

`codex` 대신 실제 사용하는 `claude` 또는 `gemini`를 지정한다. MCP를 비활성화한 수정 Cline은 `setup`을 건너뛰고 dispatcher의 CLI 브리지를 사용한다.

TUI 대신 브라우저에서 운영하려면 `autogora dashboard`를 실행하고 출력된 URL을 연다. 기본 주소는 `127.0.0.1:8420`이다. 브라우저는 URL의 일회용 토큰을 HTTP-only 세션 쿠키로 교환한다.

별도 터미널에서 실행기를 시작한다.

```bash
# 읽기·분석 작업
autogora dispatch --watch --max-workers 2 --board product-web

# 신뢰할 수 있는 코드 저장소에서 구현 작업도 허용
autogora dispatch --watch --max-workers 2 --allow-writes --board product-web
```

`--allow-writes`는 에이전트의 파일 수정과 셸 실행을 허용한다. 구현이 필요한 작업에서만, 신뢰하는 저장소를 대상으로 사용한다.

프로젝트별 보드를 먼저 만들면 작업과 실행 이력을 분리하기 쉽다.

```bash
autogora boards create product-web \
  --name "Product Web" \
  --default-workdir "$PWD" \
  --switch
```

Git 저장소를 보드의 기본 작업 디렉터리로 지정하고 작업별 경로를 생략하면 dispatcher는 run마다 분리된 detached worktree를 준비한다. task에는 작업공간 설정만 유지하고 실제 경로와 기준 commit은 run 이력에 기록한다. 현재 디렉터리를 직접 수정해야 한다면 작업 생성 시 `--workspace "$PWD" --workspace-kind dir`를 명시한다.

### GitHub issue를 Triage로 가져오기

로그인된 `gh` CLI를 통해 GitHub issue를 바로 가져올 수 있다. 먼저 변경 없이 대상을 확인한 다음 가져온다.

```bash
autogora github import \
  --repo nn1a/autogora \
  --label bug \
  --search "no:assignee sort:created-asc" \
  --limit 20 \
  --dry-run

autogora github import \
  --repo nn1a/autogora \
  --label bug \
  --search "no:assignee sort:created-asc" \
  --limit 20
```

특정 issue만 가져올 때는 `--issue`를 반복해서 사용한다. `--issue`는 `--label`, `--search`, `--state`와 함께 사용할 수 없다.

```bash
autogora github import --repo nn1a/autogora --issue 42 --issue 57
```

GitHub Enterprise Server는 host를 포함한 저장소 이름을 `gh`에 전달한다. 해당 host에 먼저 로그인하거나 `GH_ENTERPRISE_TOKEN`을 설정한다.

```bash
gh auth login --hostname github.corp.example
autogora github import \
  --host github.corp.example \
  --repo platform/control \
  --tenant platform \
  --board product-web
```

가져온 task는 `Triage`에서 시작한다. 본문과 URL 첨부에 원본 issue 주소가 남는다. 같은 issue를 다시 가져오면 활성 task를 중복 생성하지 않는다. dispatcher의 auto-decompose가 켜져 있으면 다음 tick에서 해당 task를 구체화한다.

### Web UI에서 한 작업을 끝내는 순서

1. `Triage` 컬럼 제목의 `+`를 눌러 요청을 등록한다.
2. 생성된 카드를 눌러 상세 패널을 연다.
3. 한 작업이면 `Specify`, 여러 역할로 나눌 일이면 `Decompose`를 누른다.
4. `Todo` 카드에서 Assignee, Runtime, Priority와 Description을 확인한 뒤 `Save changes`를 누른다. 실행 조건을 충족하면 `Ready`로 전환된다.
5. 여전히 `Todo`이고 `Promote` 버튼이 보일 때만 승격한다. 승격 후에도 `Todo`라면 미완료 dependency나 빠진 담당자를 확인한다.
6. 읽기·분석 작업은 상단의 `Dispatch now`로 한 번 실행할 수 있다.
7. 카드가 `Running`이면 상세 화면의 Run history와 Recent events를 확인한다.
8. 성공한 작업은 `Done`에서, 사람의 결정이 필요한 작업은 `Blocked`에서 확인한다.

Web UI의 `Dispatch now`는 기본적으로 `allowWrites=false`인 읽기 전용 1회 실행이다. 코드나 문서 파일을 수정해야 하는 작업은 별도 터미널에서 다음과 같이 실행한다.

```bash
autogora dispatch --once --allow-writes --board product-web
```

### TUI에서 보드를 운영하는 방법

프로젝트 디렉터리에서 `autogora tui`를 실행한다. 다른 보드를 열려면 `--board <slug>`를 붙인다. 화면은 2초마다 갱신되며 선택한 task ID를 유지한다.

| 키 | 동작 |
| --- | --- |
| 방향키, `h/j/k/l` | 컬럼과 카드 이동 |
| `/` | task 전체 필드 검색. `Enter`로 적용하고 `Esc`로 취소 |
| `f` | Tenant, Assignee, Runtime 필터 |
| `Tab`, `1`~`3` | Overview, Relations, Activity 전환 |
| `Page Up`, `Page Down` | 긴 상세 내용 스크롤 |
| `a` | Archived 컬럼 표시 또는 숨김 |
| `Space` | 선택한 task의 action palette |
| `n` | 현재 컬럼에 task 생성 |
| `e`, `s`, `C` | 전체 편집 폼, Agent 섹션, 댓글 추가 |
| `m` | 상태 선택 메뉴 |
| `p`, `u` | Promote, Unblock |
| `b`, `c`, `x` | Block, Complete, Archive |
| `r`, `?`, `q` | 즉시 갱신, 도움말, 종료 |

생성·편집 폼은 세 섹션으로 구성된다.

| 섹션 | 필드 |
| --- | --- |
| Task | Title, Description, Status, Priority |
| Agent | Board profile, Assignee, Runtime, Skills |
| Execution | Tenant, Workspace kind/path, Goal mode |

`Tab`과 `Shift+Tab`으로 필드를 이동하고, `Ctrl+←/→`로 섹션을 바꾼다. Status, Board profile, Runtime, Workspace kind에 포커스한 뒤 `↑/↓`로 값을 선택하고, Goal mode는 `Space`로 전환한다. 폼에도 현재 필드의 조작 키가 표시된다. `Ctrl+S`로 저장하고 `Esc`로 취소한다. Board profile은 Web API와 같은 board metadata 및 기존 task route에서 읽는다. Profile을 선택하면 Assignee와 Runtime이 함께 설정된다.

`Space` action palette에서는 Specify, Decompose, Promote, Unblock, 선택 작업 실행, 활성 run 종료, Schedule, Block, Complete, hierarchy와 dependency 편집, 첨부, Archive, Delete를 실행할 수 있다. 메뉴 항목이나 관계 대상이 많으면 메뉴 안에서 `/`를 눌러 검색한다.

Planner action과 중요한 상태 변경은 확인창에 표시된 task ID를 기준으로 실행한다. 확인하는 동안 자동 갱신이나 선택 변경이 일어나도 다른 task에 적용되지 않는다. `Running` task에서는 Title, Description, Priority처럼 소유권과 무관한 필드만 수정할 수 있다. Assignee, Runtime, Workspace, Status를 바꾸려면 action palette에서 활성 run을 먼저 종료한다.

TUI와 Web UI는 같은 task service, SQLite DB, board profile, planner 설정을 사용한다. 한 화면에서 저장한 변경은 다른 화면의 다음 갱신에 나타난다. 여러 task 일괄 변경, board 설정, swarm 생성, dispatcher 시작은 Web UI나 CLI를 사용한다.

## 3. 상태를 읽는 법

| 상태 | 의미 | 일반적인 진입 | 다음 행동 |
| --- | --- | --- | --- |
| `Triage` | 아직 범위·완료 조건·담당 경로가 불명확한 요청 | `create --triage` 또는 UI에서 Triage 선택 | `Specify` 또는 `Decompose` |
| `Todo` | 실행 규격은 있지만 담당자, 런타임, 의존성 또는 수동 승인이 남음 | specify, 미완료 선행 작업 연결, 미할당 작업 | 필드 보완 후 `Promote` |
| `Scheduled` | 미래 시각 또는 수동 재개까지 보류 | `schedule`, `--scheduled-at` | 시간이 되면 자동 승격하거나 `Promote` |
| `Ready` | 지금 claim할 수 있는 실행 대기 작업 | 담당자·비수동 런타임·의존성 조건 충족 | dispatcher가 claim |
| `Running` | worker가 원자적으로 claim한 작업 | dispatcher 또는 `claim` | heartbeat 후 `Complete` 또는 `Block` |
| `Blocked` | 사람의 입력, 기능 부족, 일시 장애 등으로 진행 불가 | `block` | 원인 해결 후 `Unblock` |
| `Review` | 자동 실행 단계가 아닌 수동 검토 보류 레인 | UI 이동 또는 `edit --status review` | 승인 시 `Complete`, 재작업 시 `Promote` |
| `Done` | 결과와 검증 근거가 남은 완료 상태 | worker `complete` 또는 사람의 완료 처리 | 필요하면 `Archive` |
| `Archived` | 기본 보드에서 숨긴 보관 상태 | `archive` | 일반 목록에서는 제외 |

`Ready` 전환 조건은 다음과 같다.

- `assignee`를 지정했다.
- 런타임이 `manual`이 아니라 `claude`, `codex`, `cline`, `gemini` 중 하나다.
- 모든 prerequisite handoff를 완료했다.
- 예약 시간이 지났거나 예약이 없다.

상태를 직접 수정해서 `Running`으로 진입할 수는 없다. claim이 실행 ID, lease, claim token을 함께 만들어야 한다.

### 관계 모델과 실행 순서

관계는 두 종류다.

| 관계 | 의미 | 실행 순서에 미치는 영향 |
| --- | --- | --- |
| Parent task → Subtask | 어떤 목표에 속한 하위 작업인지 나타내는 관계 | 직접적인 claim 차단 없음 |
| Prerequisite → Dependent | 선행 작업 결과를 어떤 후속 작업이 소비하는지 나타내는 관계 | 모든 prerequisite handoff가 끝나야 dependent가 `Ready`로 이동 |

Prerequisite 완료 시각은 dependency edge의 handoff로 남는다. 이후 선행 task를 보관하거나 다시 열어도 이미 소비한 handoff는 유효하다. dependent가 새 결과를 기다려야 한다면 dependency를 unlink한 뒤 다시 link한다.

실행 중인 dependent에는 미완료 prerequisite를 추가할 수 없다. 완료한 prerequisite는 실행을 중단하지 않고 연결할 수 있다.

`Decompose`로 만든 작업은 원본 Triage 카드의 subtask가 되고 planner의 dependency DAG는 별도로 저장된다. DAG 진입점은 병렬로 `Ready`에 진입하며, 후속 subtask는 선행 handoff가 끝날 때까지 `Todo`에 머문다. 모든 말단 subtask가 끝나면 원본 root task가 마지막 종합·검증 단계로 `Ready`에 들어간다.

예를 들어 다음 관계는 hierarchy 하나와 dependency 세 개를 가진다.

```text
Parent task: 릴리스 완료
├─ Subtask: API 계약 검토       Phase 1
├─ Subtask: API 구현            Phase 2
└─ Subtask: 회귀 리뷰           Phase 3

실행 dependency:
외부 승인 → API 계약 검토 → API 구현 → 회귀 리뷰 → Parent task
```

CLI에서 관계를 확인하고 관리할 수 있다.

```bash
# 현재 작업과 연결된 hierarchy, dependency, phase 확인
autogora graph <task-id>

# hierarchy만 설정하거나 제거한다. 실행 순서는 바뀌지 않는다.
autogora subtask-add <parent-task-id> <subtask-id> --position 0
autogora subtask-rm <parent-task-id> <subtask-id>

# 실행 dependency를 추가한다. 첫 번째 ID가 prerequisite다.
autogora link <prerequisite-id> <dependent-id>
autogora unlink <prerequisite-id> <dependent-id>
```

작업 생성 시 사용하는 `--parent <id>`는 이름과 달리 prerequisite dependency를 추가한다. hierarchy parent는 `subtask-add`로 설정한다.

MCP에서는 `autogora_graph`, `autogora_subtask_set`, `autogora_subtask_remove`, `autogora_link`, `autogora_unlink`를 사용한다. 응답의 `parents`/`children` 필드는 각각 `prerequisites`/`dependents`를 뜻한다. hierarchy는 `parentTask`/`subtasks` 필드를 사용한다.

모든 worker에게 전체 graph의 상세 데이터를 전달하지 않는다. 실행 worker가 받는 범위는 다음과 같다.

- root 목표와 현재 subtask 본문
- 현재 phase와 claim 순서 규칙
- 완료된 직접 prerequisite의 summary와 metadata
- 완료 시 열리는 직접 dependent
- 같은 workflow node의 ID, 제목, 상태, phase 요약

다른 subtask의 본문, workspace, 첨부파일, 미완료 결과는 전달하지 않는다. orchestrator나 관리 화면에서 전체 topology를 변경한다. worker는 자신이 claim한 node만 구현한다.

연결된 graph가 500개 node를 넘어도 worker는 정상적으로 시작한다. `autogora_graph`와 Web UI는 focus task, hierarchy root, 직접 관계를 우선해 최대 500개를 보여준다. `totalConnectedNodes`, `truncated`, `omittedNodeCount`에서 생략 범위를 확인할 수 있다. worker context는 필요한 node 50개의 요약만 사용한다.

![Task hierarchy와 실행 dependency를 분리해 보여주는 실제 화면](images/workflow-05-task-relationships.png)

*위쪽 phase 목록은 실행 순서를, Task hierarchy는 소속을, Execution dependencies는 claim을 차단하는 선행 관계를 보여준다.*

![계획 단계의 실제 작업 보드](images/workflow-01-board-planning.png)

*계획 구간의 카드에서 상태, 담당자, 런타임, 우선순위와 갱신 시각을 함께 확인한다.*

### Review에 대한 중요한 차이

일반 worker가 `autogora_complete`를 호출하면 카드가 `Running`에서 바로 `Done`으로 이동한다. `Review`는 자동 품질 게이트가 아니라 사람이 카드를 잠시 보류하는 레인이다.

필수 리뷰가 필요하면 구현 카드를 억지로 `Review`로 옮기기보다 별도의 리뷰 카드를 만들고 구현 카드를 prerequisite로 연결한다.

```text
분석 작업 ──> 구현 작업 ──> 리뷰 작업
   Done          Done       Ready → Running → Done
```

구현 완료 요약과 metadata는 리뷰 worker의 prerequisite handoff로 전달된다.

## 4. 좋은 카드 작성 규칙

좋은 카드는 worker가 이전 대화를 보지 않아도 실행할 수 있다. 본문에는 최소한 다음 항목을 넣는다.

```markdown
## 목표
사용자가 얻게 될 결과를 한 문장으로 적는다.

## 범위
- 변경하거나 조사할 대상
- 하지 않을 일

## 완료 조건
- 사용자가 관찰할 수 있는 동작
- 생성해야 할 파일 또는 산출물
- 실패·경계 조건

## 검증
- 실행할 테스트 또는 검사 명령
- 리뷰할 diff, 로그, 문서 링크

## 제약
- 수정 금지 영역
- 호환성, 보안, 성능 요구사항
```

제목은 행동보다 결과를 나타내는 편이 좋다.

| 피해야 할 제목 | 권장 제목 |
| --- | --- |
| 버튼 작업 | 검색 필터를 한 번에 초기화하는 버튼 추가 |
| 인증 확인 | 토큰 갱신 흐름과 실패 경로 문서화 |
| 코드 리뷰 | CSV 내보내기 변경의 회귀 위험 검증 |

### Specify와 Decompose 선택 기준

`Specify`를 선택하는 경우:

- 한 worker가 한 workspace에서 끝낼 수 있다.
- 산출물이 하나이고 완료 조건이 동일하다.
- 병렬화보다 단순한 handoff가 중요하다.

`Decompose`를 선택하는 경우:

- 분석, 구현, 검증처럼 역할이 분명히 다르다.
- 서로 독립적으로 병렬 수행할 하위 작업이 있다.
- 후속 작업이 앞선 결과를 명시적으로 소비해야 한다.

작업이 작다면 억지로 쪼개지 않는다. 작은 카드가 너무 많으면 handoff 비용과 상태 관리 비용이 실제 작업보다 커진다.

## 5. Triage에서 Done까지 단계별 운영

### 5.1 Triage: 요청을 잃어버리지 않게 등록

Web UI에서는 `New task`를 누르고 Status를 `Triage`로 선택한다. 아직 담당자를 모르면 비워 두고, 요청 원문과 결정되지 않은 항목을 Description에 남긴다.

CLI 예제:

```bash
autogora create "CSV 내보내기 요구사항 정리" \
  --triage \
  --body "현재 검색 결과를 CSV로 내려받아야 한다. 포함 컬럼과 파일명 규칙은 아직 미정이다." \
  --priority 8 \
  --tenant dashboard
```

MCP를 사용하는 Claude/Codex 대화 예제:

```text
Autogora MCP를 사용해서 아래 요청을 product-web 보드의 triage 카드로 등록해줘.
아직 구현하지 말고, 결정되지 않은 사항과 기대 결과를 카드 본문에 구분해서 남겨줘.

요청: 현재 검색 결과를 CSV로 내려받는 기능이 필요하다.
```

![Triage 카드의 실제 상세 화면](images/workflow-02-triage-detail.png)

*Triage 상세 화면에서는 요청을 수정하고 `Specify` 또는 `Decompose`를 선택할 수 있다.*

### 5.2 Specify: 한 worker가 실행할 수 있게 구체화

보조 planner에게 규격 작성을 맡긴다.

```bash
autogora specify <task-id> --planner-runtime codex
```

Cline을 planner로 사용할 수도 있다.

```bash
autogora specify <task-id> --planner-runtime cline
```

외부 planner 없이 사람이 확정한 규격을 그대로 넣으려면 `--title`과 `--body`를 함께 사용한다.

```bash
autogora specify <task-id> \
  --title "필터 적용 결과를 CSV로 내보내기" \
  --body $'## 목표\n현재 필터 결과를 CSV로 다운로드한다.\n\n## 완료 조건\n- 화면에 Export CSV 버튼이 있다.\n- 현재 필터 결과만 포함한다.\n- UTF-8 CSV를 생성한다.\n- 빈 결과에서도 헤더를 포함한다.\n\n## 검증\n- 단위 테스트와 브라우저 동작을 확인한다.'
```

`Specify`가 끝나면 카드는 `Todo`로 이동한다. 규격을 확인한다.

```bash
autogora show <task-id>
```

### 5.3 Decompose: 분석·구현·검증 그래프 만들기

```bash
autogora decompose <task-id> \
  --planner-runtime codex \
  --profile "analyst:codex:코드와 테스트를 읽고 근거를 남긴다" \
  --profile "implementer:codex:기능을 구현하고 테스트한다" \
  --profile "reviewer:claude:변경을 독립적으로 검증한다" \
  --default-profile analyst:codex \
  --orchestrator-profile implementer:codex
```

planner가 만든 그래프는 한 트랜잭션으로 적용되며 순환 의존성은 거부된다. 제목, 담당자, 런타임과 의존성 방향을 검토한다.

```bash
autogora show <root-task-id>
autogora list --sort status
```

`prerequisite → dependent`는 선행 handoff를 마쳐야 후속 task가 `Ready`로 이동한다는 뜻이다.

```bash
autogora link <prerequisite-id> <dependent-id>
autogora unlink <prerequisite-id> <dependent-id>
```

### 5.4 Todo: 실행 경로 확정

`Todo`에서 다음 항목을 확인한다.

- 누가 실행하는가: `assignee`
- 어떤 실행기를 쓰는가: `runtime`
- 어디서 작업하는가: `workspace`, `workspace_kind`, 필요하면 `branch`
- 무엇을 제출하는가: 완료 조건과 artifact 경로
- 무엇이 먼저 끝나야 하는가: prerequisite dependency

필드를 수정한다.

```bash
autogora edit <task-id> \
  --assignee implementer \
  --runtime codex \
  --workspace-kind worktree \
  --branch feat/csv-export \
  --priority 10
autogora show <task-id>
```

필드 저장 시 실행 조건을 충족하면 `Ready`로 전환된다. `Specify` 직후처럼 카드가 여전히 `Todo`에 있을 때만 직접 승격한다.

```bash
autogora promote <task-id>
```

승격 후에도 `Todo`라면 미완료 prerequisite, 빠진 담당자, `manual` 런타임을 확인한다.

### 5.5 Ready와 Running: dispatcher에 실행 위임

실행 전 후보를 미리 확인할 수 있다.

```bash
autogora dispatch --dry-run --max 3 --board product-web
```

한 건만 실행한다.

```bash
autogora dispatch --once --allow-writes --board product-web
```

지속 실행한다.

```bash
autogora dispatch --watch \
  --max-workers 2 \
  --max-in-progress 2 \
  --max-per-assignee 1 \
  --allow-writes \
  --board product-web
```

dispatcher는 claim, workspace 준비, worker 실행, heartbeat lease, 제한 시간, 재시도와 종료 상태를 관리한다. worker는 시작할 때 카드와 prerequisite handoff를 읽고, 오래 걸리는 작업 중에는 heartbeat를 남겨야 한다.

![Running 카드의 실제 상세 화면](images/workflow-03-running-detail.png)

*Running 상세 화면에서 담당자, 런타임, 우선순위와 현재 작업 내용을 확인할 수 있다.*

실행 이력과 로그를 확인한다.

```bash
autogora runs <task-id>
autogora log <task-id> --tail-bytes 65536
autogora tail <task-id> --follow
# 비정상 worker를 종료하고 run을 안전하게 회수
autogora terminate <task-id> --reason "관리자 수정 전 실행 종료"
```

![실행 이력과 heartbeat가 표시된 실제 화면](images/workflow-03-running-history.png)

*상세 화면 아래쪽의 Run history와 Recent events에서 claim과 heartbeat를 확인할 수 있다. 비정상 실행은 여기서 종료할 수 있다.*

### 5.6 Blocked: 막힌 이유와 다음 행동을 분리

worker가 계속 진행할 수 없을 때는 이유와 종류를 남긴다.

```bash
autogora block <task-id> \
  "샌드박스 API 키와 웹훅 URL 결정이 필요합니다" \
  --kind needs_input
```

- `dependency`: 다른 작업 결과를 기다리며 `Blocked`가 아닌 `Todo`로 돌아간다.
- `needs_input`: 사람의 결정이나 자격 증명이 필요하다.
- `capability`: 현재 runtime이나 도구로 수행할 수 없다.
- `transient`: 일시적인 외부 장애다.

해결한 내용은 comment로 남긴 뒤 재개한다.

```bash
autogora comment <task-id> \
  "샌드박스 키를 비밀 저장소에 등록했고 웹훅 URL을 확정했습니다" \
  --author product-owner
autogora unblock <task-id>
```

같은 원인으로 반복해서 막히면 카드는 다시 `Triage`로 올라갈 수 있다. 이때는 단순 재시도보다 요구사항이나 실행 경로를 다시 설계한다.

### 5.7 Review: 수동 보류 또는 별도 리뷰 카드

단순한 사람 승인만 필요하면 실행 중이 아닌 카드를 `Review`에 둘 수 있다.

```bash
autogora edit <task-id> --status review
```

승인하면 검증 요약과 함께 완료한다.

```bash
autogora complete <task-id> \
  --summary "요구사항과 테스트 결과를 확인했고 배포 가능한 상태입니다" \
  --metadata '{"verification":["npm test","manual review"],"residual_risk":[]}'
```

재작업이 필요하면 comment를 남기고 다시 실행 대기열로 보낸다.

```bash
autogora comment <task-id> \
  "빈 결과에서 CSV 헤더가 누락됩니다. 회귀 테스트를 추가해 주세요" \
  --author reviewer
autogora promote <task-id>
```

`Running` 카드를 바로 `Review`, `Done`, `Blocked`, `Archived`로 바꾸거나 삭제할 수는 없다. 먼저 Web UI의 Run history, CLI `terminate`, 또는 MCP `autogora_run_terminate`로 활성 worker에 종료 신호를 보낸다. PID에 신호를 보냈다면 dispatcher는 프로세스 종료를 확인할 때까지 카드를 `Running`에 두고 응답에 `pending: true`를 표시한다. 이 과정이 기존 worker와 대체 worker의 중복 실행을 막는다. PID가 없거나 프로세스가 이미 끝났다면 즉시 run을 회수한다.

실행 중에는 제목·본문·우선순위 설명만 보완할 수 있으며 assignee, runtime, workspace, branch는 바꿀 수 없다. 필수 코드 리뷰에는 별도 리뷰 카드를 사용한다.

### 5.8 Done: 결과보다 검증 가능한 handoff를 남김

dispatcher가 실행한 일반 worker는 성공 시 `autogora_complete`, 진행 불가 시 `autogora_block` 중 하나로 정확히 한 번 종료해야 한다. 완료 handoff에는 다음 정보를 권장한다.

```json
{
  "summary": "CSV 내보내기와 빈 결과 헤더 처리를 구현했습니다.",
  "metadata": {
    "changed_files": ["web/export.js", "test/export.test.ts"],
    "verification": ["npm test", "manual browser export"],
    "residual_risk": []
  },
  "artifacts": []
}
```

파일 artifact를 선언하려면 모든 경로가 workspace 안에 실제로 있어야 한다. 다음 명령으로 완료 handoff를 확인한다.

```bash
autogora show <task-id>
autogora runs <task-id>
```

![Blocked, Review, Done 상태의 실제 보드](images/workflow-04-outcome-states.png)

*Blocked에서는 해결할 원인을, Review에서는 검토 담당자를, Done에서는 완료 결과를 확인한다.*

`Done`은 결과와 검증 근거를 남긴 완료 상태다. 카드를 숨길 목적으로 `Done`으로 바꾸지 않는다. 이미 완료한 카드는 필요할 때 `Archive`로 보관한다.

## 6. 예제 1: 간단한 기능 구현

목표: 검색 조건을 한 번에 초기화하는 버튼을 구현한다.

### 1단계: Triage 등록

```bash
autogora create "검색 필터 초기화 버튼 구현" \
  --triage \
  --body "검색어와 tenant/assignee 필터를 한 번에 초기화해야 한다. 모바일 레이아웃도 유지해야 한다." \
  --assignee implementer \
  --runtime codex \
  --workspace-kind worktree \
  --branch feat/reset-search-filter \
  --priority 10
```

출력의 `task.id`를 저장한다.

```bash
TASK_ID=<task-id>
```

### 2단계: 규격화하고 검토

```bash
autogora specify "$TASK_ID" --planner-runtime codex
autogora show "$TASK_ID"
```

다음 완료 조건이 본문에 있는지 확인한다.

- 초기화 버튼의 위치와 레이블
- 검색어, tenant, assignee가 모두 초기화됨
- 초기화 후 카드 목록이 즉시 갱신됨
- 키보드 포커스와 모바일 레이아웃 유지
- 관련 테스트와 브라우저 검증

### 3단계: 실행

```bash
autogora promote "$TASK_ID"
autogora dispatch --once --allow-writes --board product-web
```

### 4단계: 결과 확인

```bash
autogora show "$TASK_ID"
autogora runs "$TASK_ID"
autogora log "$TASK_ID"
```

카드가 `Done`인지, 변경 파일과 실행한 테스트가 completion metadata에 남았는지 확인한다.

MCP 대화로 같은 흐름을 요청하는 예제:

```text
Autogora MCP를 사용해 "검색 필터 초기화 버튼" 요청을 triage로 등록해줘.
한 작업으로 끝낼 수 있으면 specify하고, implementer/codex에 배정해줘.
완료 조건에는 키보드 접근성, 모바일 레이아웃, 관련 테스트를 포함해줘.
계획만 만들고 직접 구현하지는 마.
```

## 7. 예제 2: 코드 분석 후 문서 생성

목표: 인증 코드를 먼저 분석하고, 분석 결과를 사용해 `docs/AUTH_FLOW.md`를 작성한 뒤 정확성을 검토한다.

이 예제는 예측 가능한 세 개의 카드를 직접 만든다. 동일 저장소를 순차적으로 사용하므로 의존성을 반드시 연결한다.

### 1단계: 분석 카드

```bash
autogora create "인증 토큰 갱신 흐름 분석" \
  --body $'## 목표\n인증 모듈의 토큰 갱신 흐름을 코드 근거와 함께 분석한다.\n\n## 범위\n- 진입점과 호출 순서\n- 성공/실패/재시도 경로\n- 자격 증명과 로그의 보안 경계\n- 관련 테스트와 누락된 테스트\n\n## 완료 조건\n- 코드는 수정하지 않는다.\n- 완료 요약에 파일 경로와 핵심 근거를 남긴다.' \
  --assignee analyst \
  --runtime codex \
  --workspace "$PWD" \
  --workspace-kind dir
```

```bash
ANALYSIS_ID=<분석-task-id>
```

### 2단계: 문서 생성 카드

```bash
autogora create "인증 흐름 문서 작성" \
  --body $'## 목표\n선행 분석 handoff와 실제 코드를 근거로 docs/AUTH_FLOW.md를 작성한다.\n\n## 완료 조건\n- 진입점, 정상 흐름, 실패 흐름, 보안 주의사항을 포함한다.\n- Mermaid 또는 텍스트 순서도를 포함한다.\n- 코드와 다른 추측은 쓰지 않는다.\n- 문서 링크와 검증 내용을 완료 요약에 남긴다.' \
  --assignee writer \
  --runtime claude \
  --workspace "$PWD" \
  --workspace-kind dir \
  --parent "$ANALYSIS_ID"
```

```bash
DOC_ID=<문서-task-id>
```

분석 카드가 끝날 때까지 문서 카드는 `Todo`에 머문다. 분석을 마치면 completion summary와 metadata가 문서 worker의 `Prerequisite handoffs`에 포함되고 문서 카드는 `Ready`로 전환된다.

### 3단계: 문서 검토 카드

```bash
autogora create "인증 흐름 문서 정확성 검토" \
  --body $'## 목표\ndocs/AUTH_FLOW.md를 실제 코드 및 테스트와 대조한다.\n\n## 완료 조건\n- 검토 중 파일은 수정하지 않는다.\n- 잘못된 호출 순서나 누락된 실패 경로가 없는지 확인한다.\n- 문서 링크가 유효한지 확인한다.\n- 승인 시 검증 근거와 함께 완료한다.\n- 불일치가 있으면 정확한 파일/구간과 수정 요구를 남기고 block한다.' \
  --assignee reviewer \
  --runtime codex \
  --workspace "$PWD" \
  --workspace-kind dir \
  --parent "$DOC_ID"
```

### 4단계: 순차 실행

```bash
autogora dispatch --watch --max-workers 1 --allow-writes --board product-web
```

공유 `dir` workspace의 쓰기 run에는 배타 lease가 걸린다. 같은 실제 경로나 Git 저장소를 쓰는 다른 run은 실패 횟수를 늘리지 않고 재예약된다. 이 예제에서는 분석 → 문서 → 리뷰 dependency가 논리적인 실행 순서도 함께 보장한다.

진행 상황을 확인한다.

```bash
autogora list --sort status
autogora context "$DOC_ID"
autogora diagnostics
```

## 8. 예제 3: 분석 → 구현 → 독립 리뷰

목표: API 타임아웃 문제를 분석하고 수정한 뒤 별도 reviewer가 회귀 위험을 검증한다.

### 분석

```bash
autogora create "API 타임아웃 원인 분석" \
  --body "재현 경로, 호출 스택, timeout 설정, 재시도 동작과 관련 테스트를 분석한다. 코드는 수정하지 않고 근거를 완료 요약에 남긴다." \
  --assignee analyst --runtime codex \
  --workspace "$PWD" --workspace-kind dir
```

```bash
ANALYSIS_ID=<분석-task-id>
```

### 구현

```bash
autogora create "API 타임아웃 처리 개선" \
  --body "선행 분석 결과를 기반으로 최소 변경을 구현한다. timeout과 retry의 상호작용을 테스트하고 npm test를 통과시킨다. 변경 파일, 테스트, 잔여 위험을 완료 metadata에 남긴다." \
  --assignee implementer --runtime codex \
  --workspace "$PWD" --workspace-kind dir \
  --parent "$ANALYSIS_ID"
```

```bash
IMPLEMENT_ID=<구현-task-id>
```

### 리뷰

```bash
autogora create "API 타임아웃 변경 독립 리뷰" \
  --body "선행 구현의 diff와 테스트 결과를 검토하되 파일은 수정하지 않는다. 오류 분류, 재시도 중복, 취소 신호, 기존 API 호환성을 확인한다. 승인 시 근거와 함께 complete하고, 결함이 있으면 파일/조건/재현법을 포함해 block한다." \
  --assignee reviewer --runtime claude \
  --workspace "$PWD" --workspace-kind dir \
  --parent "$IMPLEMENT_ID"
```

```bash
REVIEW_ID=<리뷰-task-id>
```

dispatcher를 시작하면 분석 카드만 `Ready`에 있다. 각 prerequisite가 끝나면 다음 카드가 `Ready`로 전환된다.

```bash
autogora dispatch --watch --max-workers 1 --allow-writes --board product-web
```

리뷰가 결함 때문에 `Blocked`되면 재작업 카드를 만들고 리뷰 카드의 새 prerequisite로 연결한다.

```bash
autogora create "리뷰 지적 타임아웃 회귀 수정" \
  --body "리뷰 카드의 block reason을 재현하고 수정한다. 집중 테스트와 전체 테스트를 실행한다." \
  --assignee implementer --runtime codex \
  --workspace "$PWD" --workspace-kind dir \
  --parent "$IMPLEMENT_ID"
```

```bash
FIX_ID=<수정-task-id>
autogora link "$FIX_ID" "$REVIEW_ID"
autogora unblock "$REVIEW_ID"
```

수정 카드가 끝날 때까지 리뷰 카드는 `Todo`에 머문다. 수정이 끝나면 리뷰 카드가 다시 `Ready`로 전환되어 독립 검증을 이어간다.

## 9. MCP 사용자용 요청 문장 모음

### 요청만 등록

```text
Autogora MCP를 사용해 이 요청을 triage에 등록해줘.
기존 중복 카드를 먼저 검색하고, 구현이나 claim은 하지 마.
요청 원문, 미결정 사항, 기대 결과를 본문에 나눠서 써줘.
```

### 실행 가능한 한 카드로 정리

```text
<task-id> triage 카드를 specify해줘.
범위, 제외 범위, 완료 조건, 검증 명령, 예상 산출물을 명확히 작성하고
결과가 Todo로 이동했는지 다시 show해서 확인해줘.
```

### 분석·구현·리뷰로 분해

```text
<task-id>를 분석 → 구현 → 독립 리뷰 의존성 그래프로 decompose해줘.
analyst/codex, implementer/codex, reviewer/claude 프로필만 사용하고,
각 카드가 prerequisite completion summary만으로 이어서 작업할 수 있게 본문을 작성해줘.
생성 후 루트와 자식의 상태 및 의존성 방향을 검토해줘.
```

### 보드 상태 점검

```text
Autogora MCP로 현재 보드를 점검해줘.
오래된 Running, 원인 없는 Blocked, 담당자 없는 Todo, prerequisite가 끝났는데 Ready가 아닌 카드를 찾고
상태를 임의로 바꾸지 말고 먼저 진단 결과와 권장 조치를 알려줘.
```

## 10. 운영 체크리스트

### 작업 시작 전

- 기존 카드와 중복되지 않는가?
- 카드 하나의 결과를 한 문장으로 설명할 수 있는가?
- 완료 조건과 검증 방법이 있는가?
- assignee와 runtime을 실제로 사용할 수 있는가?
- workspace가 안전하고 의도한 저장소를 가리키는가?
- prerequisite dependency 방향이 맞는가?

### Running 중

- worker가 heartbeat를 갱신하는가?
- `runs`, `log`, `tail`에서 진행 근거를 확인할 수 있는가?
- worker가 범위를 벗어난 파일을 수정하지 않는가?
- 사람의 결정이 필요한데 무의미한 재시도를 반복하지 않는가?

### Done 전

- 완료 조건을 모두 검증했는가?
- 테스트 또는 리뷰 명령을 기록했는가?
- 변경 파일이나 문서 경로를 metadata/artifact에 남겼는가?
- 잔여 위험을 명시했는가?
- 별도 카드에서 필수 리뷰를 마쳤는가?

## 11. 자주 막히는 원인

### Todo에서 Ready로 가지 않음

```bash
autogora show <task-id>
```

- assignee가 비어 있음
- runtime이 `manual`
- 미완료 prerequisite가 있음
- 예약 시간이 남아 있음
- specify 후 아직 promote하지 않음

### Running이 오래 유지됨

```bash
autogora runs <task-id>
autogora log <task-id>
autogora diagnostics
```

heartbeat, worker PID, claim 만료 시각과 로그를 확인한다. Web UI의 Run history에서 활성 run을 종료할 수도 있다.

```bash
autogora terminate <task-id> --reason "heartbeat 정지로 관리자 종료"
```

`diagnostics`의 `terminal_prerequisite`는 완료하지 않은 선행 task를 보관했다는 뜻이다. 해당 선행 작업이 더 이상 필요 없다면 dependency를 unlink한다. 여전히 필요하다면 prerequisite를 다시 열어 완료한다. `stalled_prerequisite`는 선행 task가 `Blocked`, `Triage`, `Review`에서 사람의 처리를 기다린다는 뜻이다.

Web UI 상단의 `Needs attention (N)` 칩에서 진단 원인과 task ID를 최대 20개까지 확인할 수 있다.

### worker가 말만 하고 Done이 되지 않음

일반 worker는 최종 답변만으로 작업을 완료할 수 없다. `autogora_complete` 또는 scoped CLI `complete`를 호출해야 한다. MCP를 비활성화한 Cline과 격리된 Gemini worker는 dispatcher prompt의 CLI lifecycle bridge를 사용한다.

### Review와 Done의 경계가 모호함

- 단순 사람 승인 대기: 기존 카드를 `Review`에 보류할 수 있다.
- 반드시 독립 실행해야 하는 검증: 별도 리뷰 카드를 만든다.
- 검증 근거 없이 보드만 정리: `Done`으로 만들지 않는다.
- 이미 검증된 완료 카드를 숨김: `Archive`를 사용한다.

## 12. 관찰 명령 요약

```bash
autogora list --sort status
autogora show <task-id>
autogora context <task-id>
autogora runs <task-id>
autogora log <task-id>
autogora stats
autogora diagnostics
autogora watch --since 0 --follow
```

Autogora의 보드 상태를 작업의 기준으로 삼는다. 결정과 결과를 대화 기록에만 두지 말고 카드 본문, comment, completion summary, metadata 또는 artifact에 지속 가능한 handoff로 남긴다.
