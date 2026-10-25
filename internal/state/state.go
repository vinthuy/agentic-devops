package state

import "fmt"

type Status string

const (
	Queued           Status = "QUEUED"
	Triaging         Status = "TRIAGING"
	Rejected         Status = "REJECTED"
	Planning         Status = "PLANNING"
	NeedsInfo        Status = "NEEDS_INFO"
	AwaitingApproval Status = "AWAITING_APPROVAL"
	Coding           Status = "CODING"
	Verifying        Status = "VERIFYING"
	Reviewing        Status = "REVIEWING"
	Committing       Status = "COMMITTING"
	Pushed           Status = "PUSHED"
	PROpened         Status = "PR_OPENED"
	CIWatching       Status = "CI_WATCHING"
	Done             Status = "DONE"
	NeedsHuman       Status = "NEEDS_HUMAN"
	Failed           Status = "FAILED"
	Cancelled        Status = "CANCELLED"
)

var AllStatuses = []Status{
	Queued, Triaging, Rejected, Planning, NeedsInfo, AwaitingApproval, Coding, Verifying,
	Reviewing, Committing, Pushed, PROpened, CIWatching, Done, NeedsHuman, Failed, Cancelled,
}

func (s Status) Valid() bool {
	for _, c := range AllStatuses {
		if c == s {
			return true
		}
	}
	return false
}

// Terminal 状态不再有出边。
var Terminal = set(Rejected, Done, Failed, Cancelled)

// Suspended 状态等待外部输入（用户回复、审批或人工处理）。
var Suspended = set(NeedsInfo, AwaitingApproval, NeedsHuman)

// base 是 SDD 4.2 中的核心转换；通用的 FAILED / CANCELLED / NEEDS_HUMAN 在 build 中补充。
var base = map[Status]map[Status]bool{
	Queued:    set(Triaging),
	Triaging:  set(Rejected, Planning),
	Rejected:  set(),
	Planning:  set(NeedsInfo, AwaitingApproval, Coding),
	NeedsInfo: set(Planning),
	// 审批门不是单行道：Rejected 是「人工否决，不执行」（与 L 级的语义一致），
	// Planning 是「计划不行，带着理由重做」。只有 Coding 一条出边意味着人只能点头。
	AwaitingApproval: set(Coding, Rejected, Planning),
	Coding:           set(Verifying, NeedsInfo),
	// Verifying → Planning 用于“验收标准本身不可执行”（例如选择器匹配不到测试）：
	// 这类失败改代码无法解决，必须重新规划而不是空耗修复轮次。
	Verifying:  set(Coding, Planning, Reviewing, Committing),
	Reviewing:  set(Coding, Committing),
	Committing: set(Pushed, Done),
	Pushed:     set(PROpened),
	PROpened:   set(Done, CIWatching),
	CIWatching: set(Coding, Done),
	Done:       set(),
	NeedsHuman: resumable(),
	Failed:     set(),
	Cancelled:  set(),
}

var transitions = build()

func build() map[Status]map[Status]bool {
	out := make(map[Status]map[Status]bool, len(AllStatuses))
	for _, src := range AllStatuses {
		dsts := map[Status]bool{}
		for d := range base[src] {
			dsts[d] = true
		}
		if !Terminal[src] {
			dsts[Failed] = true
			dsts[Cancelled] = true
		}
		if !Terminal[src] && src != NeedsHuman {
			dsts[NeedsHuman] = true
		}
		out[src] = dsts
	}
	return out
}

func resumable() map[Status]bool {
	out := map[Status]bool{}
	for _, s := range AllStatuses {
		if !Terminal[s] && !Suspended[s] {
			out[s] = true
		}
	}
	// 审批门是挂起状态里的例外：审批超时会把任务升级到 NEEDS_HUMAN，
	// resume 需要能把它送回审批门继续等人。NeedsInfo 不走这条路（它有自己的 reply），
	// 所以只放开 AwaitingApproval，而不是把整个 Suspended 都塞进 resumable。
	out[AwaitingApproval] = true
	return out
}

func set(items ...Status) map[Status]bool {
	out := make(map[Status]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}

// CanTransition 判断 src -> dst 是否合法。
func CanTransition(src, dst Status) bool {
	return transitions[src][dst]
}

// AssertTransition 在非法转换时返回错误。
func AssertTransition(src, dst Status) error {
	if !CanTransition(src, dst) {
		return fmt.Errorf("非法状态转换: %s -> %s", src, dst)
	}
	return nil
}
