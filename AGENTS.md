# AGENTS.md

Coterix 프로젝트에서 작업하는 에이전트를 위한 지침입니다.

## 설계·빌드 문서 (로컬 전용)

구현 작업 전 로컬 `docs/`의 설계 문서를 먼저 읽는다. 이 문서들은 공개 저장소에 포함되지 않는다(git 미추적):

- `docs/coterix-guide.md` — 작업 규칙·경계·확정된 결정
- `docs/PLAN.md` — 정식 build contract
- `docs/BUILD.md` — 구현 마일스톤(M0~)
- `docs/spec/` — cli-config·state·ui 스펙
- `docs/prompts/` — 에이전트 CLI 프롬프트
- `docs/agent/`, `docs/human/` — 설계 근거·판단 변화 스냅샷

## Git Commit 규칙

Git 커밋 메시지를 작성할 때 다음 규칙을 **반드시** 따르세요.

- ❌ `Co-Authored-By: Claude <noreply@anthropic.com>` 라인을 **절대 포함하지 마세요**
- 커밋 메시지는 간결하고 명확하게 작성
- 형식: `type(scope): 설명` 또는 `type: 설명`
- 필요시 본문에 상세 설명 추가 (Co-Authored-By 없이)

### type 종류

- `feat`: 새로운 기능 추가
- `fix`: 버그 수정
- `docs`: 문서 변경
- `refactor`: 리팩터링 (기능 변경 없음)
- `chore`: 빌드/설정/버전 등 기타 작업
- `test`: 테스트 추가/수정

### 예시

✅ **올바른 커밋 메시지**:

```
feat(orchestrator): 멀티 에이전트 라우팅 로직 추가

- 에이전트 우선순위 기반 디스패치 구현
- 실패 시 폴백 경로 추가
```

❌ **잘못된 커밋 메시지** (Co-Authored-By 포함):

```
feat(orchestrator): 멀티 에이전트 라우팅 로직 추가

Co-Authored-By: Claude <noreply@anthropic.com>
```
