# 번역 용어집과 문체 규칙

원 저자 코드의 중국어를 옮길 때 쓰는 용어의 정본이다(부모 이슈 #99 공통 규칙 1번). 번역 PR은 이 표의 말을 쓴다. 표에 없는 용어를 새로 정하면 같은 PR에서 이 문서에 더한다.

## 언어

| 대상 | 언어 |
|---|---|
| 코드 주석 | 한국어 |
| 화면 문구(`web/`) | 한국어. i18n 라이브러리는 쓰지 않는다 |
| 백엔드가 사용자에게 보여 주는 오류·상태 메시지, 서버 로그 | 한국어 |
| LLM 프롬프트(내장 agent 프롬프트, 승인 심사 프롬프트, 스킬 지침) | 영어. 이미 영어인 것은 그대로 둔다 |

식별자, 파일명, API 경로, JSON 키, DB 컬럼은 바꾸지 않는다. 문장 안에 식별자가 나오면 원래 철자대로 쓴다(예: `add_intent`, `traffic_refs`).

## 용어를 고르는 기준

1. 누구나 바로 알아보는 공식 용어와 보안 업계 표준 용어를 쓴다(취약점, 침투 테스트, 자산, 공격 표면).
2. 작성자만 아는 은어, 중국어를 글자대로 옮겨 어색한 말, 임의로 만든 줄임말을 쓰지 않는다.
3. 한국어 표준 용어가 어색하거나 없으면 널리 쓰는 영어 원어나 음차를 쓴다(프롬프트, 페이로드, 토큰).
4. 애매하면 KISA·OWASP 한국어 자료, 국립국어원 외래어 표기를 근거로 고르고 비고에 적는다.
5. 이미 한국어로 된 화면(LLM 프로필, ChatGPT·Claude 구독 로그인)의 말이 있으면 그 말을 따른다. 비고의 "기존 화면"이 그 뜻이다.
6. 한 개념에는 한 말만 쓴다. 같은 중국어가 문맥에 따라 다른 개념이면 문맥별로 행을 나눈다.

## 문체 규칙

### 화면 문구

- 버튼, 메뉴, 탭, 표 열 이름, 입력 칸 이름: 짧은 명사형. 예: "저장", "취소", "작업 목록", "콜백 주소", "다시 로그인".
- 버튼은 동작을 명사로 쓴다: "삭제", "새로 만들기", "코드 받기". "삭제하기"처럼 "-하기"를 붙이지 않는다.
- 안내·설명·오류 문장: 서술은 "~합니다"체, 사용자에게 할 일을 요청할 때는 "~하세요"로 끝낸다. 기존 화면이 이렇게 쓴다(예: "로그인 시간이 지났습니다. 로그인을 다시 시작하세요.").
- 상태 표시: 짧은 형태로 쓴다. 예: "연결됨", "연결 안 됨", "저장 중", "불러오는 중", "재로그인 필요".
- 문장 끝 마침표: 문장이면 찍고, 명사형 라벨이면 찍지 않는다.
- 자리표시자(placeholder)는 예시 값만 쓰거나 "예: …" 형식으로 쓴다.

### 오류 메시지

화면과 백엔드 사용자 메시지에 같은 형식을 쓴다.

- 형식: "무엇을 하지 못했습니다" + (알 수 있으면) 이유 + 할 일. 예: "자격 증명을 저장하지 못했습니다. 잠시 뒤 다시 시도하세요."
- "실패"를 문장 끝에 쓰지 않는다. "저장 실패" 대신 "저장하지 못했습니다". 짧은 상태 라벨(토스트 제목 등)에서만 "저장 실패"를 쓴다.
- 입력 검사 오류: "<칸 이름>을(를) 입력하세요", "<칸 이름>은(는) 비워 둘 수 없습니다", "<칸 이름>은(는) <조건>이어야 합니다".
- 없는 대상: "<대상>이(가) 없습니다". "존재하지 않습니다"는 쓰지 않는다.
- 서버 로그는 문장 대신 "무엇: 원인" 형태로 짧게 쓴다. 예: `log.Printf("알림 채널 읽기 실패: %v", err)`. 로그는 사용자에게 보이지 않으므로 "실패"를 써도 된다.
- Go 오류 값(`fmt.Errorf`)은 Go 관례대로 소문자 시작·마침표 없이 이어 붙일 수 있게 쓴다. 예: `fmt.Errorf("알림 채널 저장: %w", err)`.

### 테스트 메시지

- Go 관례의 "실제값, 기대값" 순서를 따른다: `t.Fatalf("<대상> = %v, 기대값 %v", got, want)`.
- "得到"는 "실제값", "期望"는 "기대값", "应为 X"는 "X여야 함"으로 옮긴다.

### 주석

- `AGENTS.md`·`coding-style.md`를 따른다. 평서문("~한다")으로 쓰고, "무엇"보다 "왜"를 쓴다.
- 원문 주석이 코드를 그대로 되풀이하기만 하면 번역하면서 짧게 줄여도 된다. 뜻을 바꾸거나 없는 내용을 더하지 않는다.

### 프롬프트(영어)

- 아래 표의 영어 열을 쓴다. 도구 이름, 필드 이름, 상태 값은 코드의 철자 그대로 둔다.
- 원문의 강조(굵게, 【】 괄호)는 Markdown 굵게나 대문자 머리말(`IMPORTANT:`)로 옮기고 구조는 유지한다.

## 용어 표

열: 중국어(원문) / 한국어(주석·화면·메시지) / 영어(프롬프트) / 비고.

### 보안 테스트 도메인

| 중국어 | 한국어 | 영어(프롬프트) | 비고 |
|---|---|---|---|
| 渗透测试 | 침투 테스트 | penetration test | KISA·업계 표준 |
| 外部渗透 | 외부 침투 테스트 | external penetration test | |
| 漏洞 | 취약점 | vulnerability | KISA 표준. "버그"·"구멍"으로 옮기지 않는다 |
| 发现 (보고된 결과) | 발견 사항 | finding | 도구 이름 `report_finding`과 맞춘다. 동사일 때는 "찾다" |
| 资产 | 자산 | asset | 보안 업계 표준 |
| 资产范围 | 자산 범위 | asset scope | |
| 资产覆盖度 | 자산 커버리지 | asset coverage | "커버리지"는 테스트 분야에서 널리 쓰는 원어 |
| 攻击面 | 공격 표면 | attack surface | OWASP 표준 용어 |
| 证据 | 증거 | evidence | |
| 流量 | 트래픽 | traffic | HTTP 요청·응답 기록을 뜻한다 |
| 流量证据 | 트래픽 증거 | traffic evidence | |
| 复现 | 재현 | reproduce | |
| 验证 | 검증 | verify / verification | |
| 已修复 | 수정됨 | fixed | 취약점 상태 |
| 复测 | 재검사 | retest | `finding_retests`와 같은 개념 |
| 严重等级 | 심각도 | severity | OWASP·CVSS 한국어 자료의 표기 |
| 严重 / 高危 / 中危 / 低危 / 信息 | 치명 / 높음 / 중간 / 낮음 / 정보 | critical / high / medium / low / info | CVSS 등급 이름. 값(`critical` 등)은 그대로 둔다 |
| 影响与危害 | 영향 | impact | |
| 注入 (공격) | 인젝션 | injection | OWASP Top 10 한국어판 표기 |
| 注入 (문맥에 넣기) | 넣다 / 주입하다 | inject | 프롬프트·컨텍스트에 넣는 뜻. 공격 이름과 구분한다 |
| 负载 / 载荷 | 페이로드 | payload | 원어 음차가 표준 |
| 范围 | 범위 | scope | 테스트 대상 범위 |
| 授权边界 | 허가된 테스트 범위 | authorized scope | |
| 目标 (테스트 대상) | 대상 | target | 예: 테스트 대상, 대상 사이트 |
| 目标 (작업 목표) | 목표 | goal | "任务目标"은 "작업 목표" |
| 域名 | 도메인 | domain | |
| 子域名 | 하위 도메인 | subdomain | 국립국어원·KISA 표기 |
| 根域名 | 루트 도메인 | root domain | |
| 网段 | 네트워크 대역 | network range (CIDR) | 값이 CIDR이면 "CIDR 대역"도 쓴다 |
| 端口 | 포트 | port | |
| 企业 | 기업 | company | 자산을 묶는 단위 |
| 官网 | 공식 웹사이트 | official website | |
| 后台 | 관리자 페이지 | admin panel | "백엔드"와 구분한다 |
| ICP备案 / 备案号 | ICP 등록 번호 | ICP filing number | 중국 웹사이트 운영 등록 번호(备案). "비안"처럼 음차하지 않는다 |
| 拦截 | 차단 | block | `intercept` 패키지의 자산 차단 규칙. #99 소유자 코멘트 표기 |
| 拦截规则 | 차단 규칙 | block rule | |
| 命中 (규칙) | 일치 | match | "命中规则"은 "일치한 규칙" |
| 命中 (캐시) | 적중 | hit | "캐시 적중" |
| 全等 / 精确匹配 | 정확히 일치 | exact match | |
| 模糊匹配 / 模糊 | 부분 일치 | partial match | 화면 예시값이 접두사·부분 문자열(`.gov.cn`, `/admin`)이다 |
| 排除 | 제외 | exclude | |
| 白名单 / 黑名单 | 허용 목록 / 차단 목록 | allowlist / blocklist | 색 이름 대신 뜻을 쓴다(OWASP 권고) |
| 约束 | 제약 조건 | constraint | 사용자가 정한 허용·금지 작업 |
| 操作约束 | 작업 제약 조건 | operation constraint | |
| 允许 / 禁止 | 허용 / 금지 | allow / deny | |
| 被动侦察 | 수동적 정보 수집 | passive reconnaissance | "수동"만 쓰면 "manual"로 읽히므로 "수동적"으로 쓴다 |
| 审批 | 승인 심사 | approval review | #103 제목과 맞춘다 |
| 实际操作 | 실제 동작 | actual action | 승인 심사 결과의 첫 항목 |
| 成功后的后果 | 성공 시 결과 | effect if successful | 승인 심사 결과의 둘째 항목 |
| 命中规则 (심사) | 적용 규칙 | matched rule | 승인 심사 결과의 셋째 항목 |
| 详细报告 | 상세 보고서 | detailed report | |
| 报告 | 보고서 | report | 동사 "上报"은 "보고하다" |
| 上报 | 보고 | report | |
| 入库 | 저장 | store | "입고"로 옮기지 않는다 |

### 에이전트·작업 구조

| 중국어 | 한국어 | 영어(프롬프트) | 비고 |
|---|---|---|---|
| 任务 | 작업 | task | 화면 "작업 목록". 식별자 `task` |
| 意图 | 탐색 의도 | intent | 탐색 그래프의 노드 하나. 짧게 "의도"도 쓴다. 식별자 `intent` |
| 事实 | 사실 | fact | 탐색 중 확인한 정보 노드 |
| 节点 | 노드 | node | |
| 探索 | 탐색 | exploration | `AGENTS.md`의 표기 |
| 规划者 / Planner | 플래너 | planner | 역할 이름은 원어 음차 |
| Worker | 워커 | worker | |
| 编排 Agent | 오케스트레이션 에이전트 | orchestration agent | |
| Agent | 에이전트 | agent | |
| 主 Agent | 메인 에이전트 | main agent | |
| 规划 | 계획 | planning | |
| 领取 | 맡기 | claim | 워커가 의도·작업을 맡는 것. 예: "의도를 맡는다" |
| 派生 | 파생 | derive | |
| 产出 | 결과물 | output | 동사일 때는 "만들어 내다" |
| 达成 | 달성 | achieve | |
| 拿到 | 얻다 | obtain | |
| 预算 (단계 수) | 단계 한도 | step budget | |
| 收尾 | 마무리 | wrap-up | |
| 压缩 | 압축 | compaction | 컨텍스트 압축 |
| 上下文 | 컨텍스트 | context | |
| 摘要 | 요약 | summary | |
| 提示词 / Prompt | 프롬프트 | prompt | |
| 提示 (안내) | 안내 / 힌트 | hint | 화면 안내문은 "안내", 에이전트에 주는 단서는 "힌트" |
| 工具 | 도구 | tool | |
| 调用 | 호출 | call | |
| 工具调用 | 도구 호출 | tool call | |
| 模型 | 모델 | model | |
| 供应商 / provider | 제공자 | provider | 기존 화면 |
| 技能 | 스킬 | skill | Claude Code 표기를 따른다 |
| 对话 | 대화 | chat | |
| 会话 | 세션 | session | |
| 记忆 | 메모리 | memory | |
| 优先级 | 우선순위 | priority | |
| 依赖 | 의존 | dependency | |
| 实验功能 | 실험 기능 | experimental feature | |

### 작업 상태와 수명 주기

| 중국어 | 한국어 | 영어(프롬프트) | 비고 |
|---|---|---|---|
| 状态 | 상태 | status / state | |
| 运行中 | 실행 중 | running | |
| 已完成 | 완료 | done | |
| 暂停 | 일시 중지 | pause | |
| 恢复 | 재개 | resume | |
| 取消 | 취소 | cancel | |
| 停止 | 중지 | stop | |
| 超时 | 시간 초과 | timeout | |
| 重试 | 재시도 | retry | |
| 轮询 | 폴링 | polling | 원어 음차가 업계 표준 |
| 宽限 | 유예 시간 | grace period | |
| 硬取消 | 강제 취소 | hard cancel | |
| 假删除 | 소프트 삭제 | soft delete | DB 업계 표준 용어 |
| 真删除 | 영구 삭제 | hard delete | |
| 级联 | 연쇄 삭제 | cascade delete | DB의 CASCADE 동작 |
| 已删除 | 삭제됨 | deleted | |
| 任务正在删除 | 작업을 삭제하는 중입니다 | task is being deleted | |
| 租约 | 선점 기한 | lease | 알림 발송기가 한 행을 잡아 두는 기한. "임대"로 옮기지 않는다 |
| 一次性 | 일회성 | one-time | |

### 알림

| 중국어 | 한국어 | 영어(프롬프트) | 비고 |
|---|---|---|---|
| 通知 | 알림 | notification | |
| 推送 | 알림 발송 | push / send | |
| 渠道 | 알림 채널 | channel | 문맥이 분명하면 "채널" |
| 投递 | 전달 | delivery | |
| 钉钉 | DingTalk | DingTalk | 제품 이름은 원어 |
| 企业微信 | WeCom | WeCom | 제품의 공식 영어 이름 |
| 飞书 | Feishu(Lark) | Feishu | |
| 事件 | 이벤트 | event | |
| 触发 | 트리거 | trigger | 화면 라벨. 문장에서는 "~할 때 실행" |

### 설정과 입력

| 중국어 | 한국어 | 영어(프롬프트) | 비고 |
|---|---|---|---|
| 配置 | 설정 | configuration / settings | |
| 默认 | 기본값 / 기본 | default | 값이면 "기본값", 수식어면 "기본" |
| 默认关 | 기본값 꺼짐 | off by default | |
| 内置 | 내장 | built-in | |
| 自定义 | 사용자 지정 | custom | Microsoft 한국어 스타일 |
| 全局 | 전역 | global | |
| 可选 | 선택 | optional | 입력 칸 라벨 "(선택)" |
| 必填 | 필수 | required | 입력 칸 라벨 "(필수)" |
| 留空 | 비워 두면 | leave empty | 예: "비워 두면 기본값을 씁니다" |
| 不限 | 제한 없음 | unlimited | |
| 上限 | 최대 | maximum / limit | 예: "최대 10개" |
| 次数 | 횟수 | count | |
| 启用 / 未启用 | 사용 / 사용 안 함 | enabled / disabled | 기능 토글 |
| 关闭 (기능) | 끄기 / 꺼짐 | disable / off | 창을 닫을 때는 "닫기" |
| 已关闭 | 꺼짐 | disabled | |
| 生效 | 적용 | take effect | |
| 覆盖 (설정) | 덮어쓰기 | override | 커버리지 뜻의 "覆盖度"와 구분한다 |
| 旧版 | 이전 버전 | legacy | |
| 版本 | 버전 | version | |
| 只读 | 읽기 전용 | read-only | |
| 不发送 | 보내지 않음 | omit | 모델 요청 필드를 보내지 않는다는 뜻 |
| 参数 | 파라미터 | parameter | HTTP 파라미터와 같은 말을 쓴다 |
| 字段 | 필드 | field | |
| 地址 | 주소 | address / URL | 기존 화면("콜백 주소") |
| 接口 | API | API | HTTP 엔드포인트일 때. Go 인터페이스 타입이면 "인터페이스" |
| 服务 | 서비스 | service | |
| 应用 | 적용 / 애플리케이션 | apply / application | 동사면 "적용", 명사면 "애플리케이션" |
| 凭据 | 자격 증명 | credential | 기존 화면 |
| 令牌 | 토큰 | token | 기존 화면 |
| 密钥 | 키 | key | "API 키" |
| 订阅 | 구독 | subscription | 기존 화면 |
| 代理 | 프록시 | proxy | 네트워크 프록시일 때 |

### 일반 화면 문구

| 중국어 | 한국어 | 영어(프롬프트) | 비고 |
|---|---|---|---|
| 保存 | 저장 | save | 버튼 |
| 保存中 | 저장 중 | saving | |
| 已保存 | 저장했습니다 | saved | 토스트는 문장으로 |
| 保存失败 | 저장하지 못했습니다 | failed to save | 오류 형식 참고 |
| 删除 | 삭제 | delete | |
| 删除失败 | 삭제하지 못했습니다 | failed to delete | |
| 新建 | 새로 만들기 | create | 버튼. 대상이 있으면 "새 <대상>" |
| 新增 | 추가 | add | |
| 创建 | 만들기 | create | 기존 화면("로그인 주소 만들기") |
| 清空 | 비우기 | clear | |
| 忽略 | 무시 | ignore | |
| 保留 | 유지 | keep | 데이터를 남기면 "보존" |
| 省略 | 생략 | omit | |
| 测试 | 테스트 | test | |
| 全部 | 전체 | all | |
| 列表 | 목록 | list | |
| 名称 | 이름 | name | |
| 描述 | 설명 | description | |
| 类型 | 유형 | type | |
| 来源 | 출처 | source | |
| 操作 (표 열) | 관리 | actions | 표의 버튼 열 이름 |
| 操作 (동작) | 작업 / 동작 | operation / action | "작업"은 task와 겹치므로 문맥이 모호하면 "동작" |
| 记录 | 기록 | record / log | |
| 日志 | 로그 | log | |
| 消息 | 메시지 | message | |
| 正文 | 본문 | body | |
| 文本 | 텍스트 | text | |
| 数字 | 숫자 | number | |
| 数据 | 데이터 | data | |
| 文件 | 파일 | file | |
| 目录 | 디렉터리 | directory | 국립국어원 외래어 표기 |
| 命令 | 명령 | command | |
| 输入 / 输出 | 입력 / 출력 | input / output | |
| 执行 | 실행 | execute / run | |
| 解析 | 파싱 | parse | DNS 이름 해석이면 "조회" |
| 请求 / 响应 | 요청 / 응답 | request / response | |
| 错误 | 오류 | error | "에러"로 쓰지 않는다 |
| 失败 | 실패 / ~하지 못했습니다 | failed | 오류 형식 참고 |
| 无效 | 잘못된 | invalid | 예: "잘못된 주소입니다" |
| 为空 / 不能为空 | 비어 있음 / 비워 둘 수 없습니다 | empty / must not be empty | |
| 不存在 | 없습니다 | not found | |
| 必须是 | ~이어야 합니다 | must be | |
| 拒绝 | 거부 | reject / deny | 기존 화면("로그인이 거부됐습니다") |
| 可用 | 사용 가능 | available | |
| 加载中 | 불러오는 중 | loading | |
| 未配置 | 설정 안 됨 | not configured | |
| 未分类 | 미분류 | uncategorized | |
| 关联 | 관련 | related | |
| 绑定 (트래픽) | 연결 | link / attach | 발견 사항에 트래픽 기록을 잇는 것 |
| 登记 | 등록 | register / record | |
| 单条 / 批量 | 단건 / 일괄 | single / batch | |
| 字节 | 바이트 | bytes | |
| 个字符 | 자 | characters | 예: "최대 120자" |
| 分钟 | 분 | minutes | |
| 时间 | 시간 | time | |
| 详细 | 상세 | details | |
| 中文技能 (테스트 입력) | 번역하지 않음 | — | 스킬 이름 검사의 비ASCII 입력값. 공통 규칙 3번 |

### 화면 기능(#111에서 더함)

web 기능 화면(`web/src/app/(main)/function/**`)을 옮기며 정한 말이다.

| 중국어 | 한국어 | 영어(프롬프트) | 비고 |
|---|---|---|---|
| 端点 / 接口 (자산 종류 `endpoint`) | 엔드포인트 | endpoint | URL 경로 자산. HTTP API 일반을 뜻하는 "接口 → API"와 구분한다 |
| C段 | /24 대역 | /24 range | CIDR 표기. 클래스 이름(C 클래스)은 쓰지 않는다 |
| 解析类型 / 解析值 | 레코드 유형 / 레코드 값 | record type / record value | DNS 표준 용어 |
| 指纹 | 핑거프린트 | fingerprint | 업계 표준 음차 |
| 绑定域名 / 开放端口 | 연결된 도메인 / 열린 포트 | bound domain / open port | |
| 归属 / 未归属 | 소속 / 소속 없음 | belongs to / unassigned | 자산이 기업에 속하는 관계. "所属任务"은 "소속 작업" |
| 利用 | 익스플로잇 | exploit | 업계 표준 원어 |
| 深入利用 / 深入 | 취약점 심화 검증 / 심화 검증 | deep verification | 발견 사항을 다시 검증하는 기능 |
| 链路 / 攻击链路图 | 공격 경로 / 공격 경로 그래프 | attack path / attack path graph | "attack path"가 업계 표준 |
| 探索链路 | 탐색 경로 | exploration path | |
| 图谱 / 态势图 | 그래프 / 현황 그래프 | graph / status graph | |
| 资产覆盖图 | 자산 커버리지 그래프 | asset coverage graph | |
| 播报 / 播报板 | 활동 피드 / 피드 | activity feed | 작업 상세의 시간순 활동 탭 |
| 黑板 | 블랙보드 | blackboard | 에이전트 구조의 표준 용어(blackboard architecture) |
| 心跳 | 하트비트 | heartbeat | |
| 故障转移 | 장애 조치 | failover | Microsoft 표준 용어 |
| 配置链 | LLM 프로필 체인 | profile chain | 기존 LLM 화면의 "프로필" |
| 额度 | 사용 한도 | quota | |
| 并发限制 | 동시 실행 제한 | concurrency limit | |
| 归档 / 已归档 | 보관 / 보관됨 | archive / archived | Microsoft 한국어 표기 |
| 还原 | 복원 | restore | |
| 冷存储 | 콜드 스토리지 | cold storage | |
| 快照 | 스냅숏 | snapshot | 국립국어원 표기 |
| 置顶 | 상단 고정 | pin | |
| 重命名 | 이름 바꾸기 | rename | Microsoft 한국어 |
| 分类 / 类别 | 분류 | category | "类型(유형)"과 구분한다 |
| 处理状态 / 待处理 | 처리 상태 / 처리 대기 | triage status / pending | |
| 已确认 / 已否定 | 확인됨 / 기각됨 | confirmed / rejected | |
| 待采纳 / 已采纳 / 已替代 | 반영 대기 / 반영됨 / 대체됨 | pending / adopted / superseded | 제약 조건 상태 |
| 生效中 | 적용 중 | active | |
| 排队中 / 已暂停 / 已创建 | 대기 중 / 일시 중지됨 / 생성됨 | queued / paused / created | |
| 待领取 | 맡기 대기 | unclaimed | "领取 → 맡기"를 따른다 |
| 重跑 | 재실행 | rerun | |
| 优雅收尾 | 정상 마무리 | graceful wrap-up | |
| 终局判定 | 최종 판정 | final verdict | |
| 态势研判 | 상황 판단 | situation assessment | |
| 系统审计 | 시스템 감사 | system audit | |
| 旁路问题 (/btw) | 별도 질문 | side question | |
| 起点 / 根任务 | 시작점 / 루트 작업 | origin / root task | |
| 移出 (작업 자산) | 작업에서 제외 | remove from task | "排除 → 제외"를 따른다 |
| 缓存读取 / 缓存写入 | 캐시 읽기 / 캐시 쓰기 | cache read / cache write | |
| 命中率 | 적중률 | hit rate | "命中(캐시) → 적중"을 따른다 |
| 入 / 缓 / 出 (토큰 줄임) | 입력 / 캐시 / 출력 | input / cache / output | 줄임말을 풀어 쓴다 |
| 录制 | 기록 | record | 트래픽·LLM 호출 기록. "录制中"은 "기록 중" |
| 延迟 | 지연 시간 | latency | |
| 方法 (HTTP) | 메서드 | method | |
| 原文 (HTTP 원본) | 원본 | raw | |
| 报文 / 数据包 | 메시지 / 패킷 | message / packet | |
| 归一化 | 정규화 | normalize | |
| 数据源 / 同步 | 데이터 소스 / 동기화 | data source / sync | |
| 不可达 | 연결할 수 없음 | unreachable | 상태 라벨 |
| 统计 / 筛选 | 통계 / 필터 | statistics / filter | |
| 导出 / 搜索·检索 / 刷新 | 내보내기 / 검색 / 새로 고침 | export / search / refresh | 버튼 |
| 折叠 / 展开 | 접기 / 펼치기 | collapse / expand | |
| 视图 | 보기 | view | 예: "그룹 보기" |
| 条 (건수) / 已选 | 건 / 선택됨 | items / selected | 예: "총 N건", "N건 선택됨" |
| 正序 / 倒序 | 오름차순 / 내림차순 | ascending / descending | |
| 上一页 / 下一页 | 이전 / 다음 | previous / next | |
| 上传 / 下载 / 文件夹 | 업로드 / 다운로드 / 폴더 | upload / download / folder | |
| 关键词 / 标签 / 备注 | 키워드 / 태그 / 메모 | keyword / tag / note | |
| 概览 | 개요 | overview | |
| 不可撤销 | 되돌릴 수 없습니다 | cannot be undone | "이 작업"은 task로 읽히므로 대상을 쓴다 |

## 고친 기록

- 2026-10-10: 처음 만든다(#101). 근거 명령과 결과는 이 문서를 더한 PR 본문에 있다.
- 2026-10-10: web 기능 화면 번역(#111)에서 정한 용어를 "화면 기능" 표로 더한다.
