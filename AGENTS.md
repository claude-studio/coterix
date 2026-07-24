# AGENTS.md

Coterix 프로젝트에서 작업하는 에이전트를 위한 지침입니다.

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
