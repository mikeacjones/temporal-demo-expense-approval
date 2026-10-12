package expense

import "time"

const (
	// DecisionSignal carries an approver's decision into a running ExpenseApproval.
	DecisionSignal = "decision"
	// StatusQuery returns the Workflow's current Status.
	StatusQuery = "status"
)

// ExpenseRequest is the input to ExpenseApproval.
type ExpenseRequest struct {
	ExpenseID   string
	Employee    string
	Description string
	Category    string
	AmountCents int64

	// Approver decides first; EscalationApprover can also decide once the
	// request has been escalated.
	Approver           string
	EscalationApprover string

	// How long to wait for the approver before escalating, and how long to wait
	// in total before the request expires. In production these would be days;
	// the demo passes seconds so the escalation is visible during a talk.
	EscalateAfter time.Duration
	ExpireAfter   time.Duration

	Demo DemoOptions
}

// DemoOptions are failure-injection settings read only by Activities, so the
// Workflow itself reads like production code.
type DemoOptions struct {
	// PaymentFailures makes the first N attempts of PayReimbursement fail, as if
	// the payment provider were briefly unavailable.
	PaymentFailures int
}

// Decision is an approver's response. Keeping who, why, and when (not just a
// boolean) gives the Event History a complete audit trail.
type Decision struct {
	Approved  bool
	By        string
	Comment   string
	DecidedAt time.Time
}

// PolicyResult is the outcome of the automated expense-policy check.
type PolicyResult struct {
	AutoApproved bool
	Rejected     bool
	Reason       string
}

// Outcome is how an expense request finished.
type Outcome string

const (
	OutcomeAutoApproved   Outcome = "auto-approved"
	OutcomeApproved       Outcome = "approved"
	OutcomeRejected       Outcome = "rejected"
	OutcomePolicyRejected Outcome = "policy-rejected"
	OutcomeExpired        Outcome = "expired"
)

// ExpenseResult is the Workflow's return value.
type ExpenseResult struct {
	Outcome    Outcome
	Reason     string
	Decision   *Decision `json:",omitempty"`
	PaymentRef string    `json:",omitempty"`
}

// Stage is where the request is in the approval process.
type Stage string

const (
	StageCheckingPolicy   Stage = "checking-policy"
	StageAwaitingApproval Stage = "awaiting-approval"
	StageEscalated        Stage = "escalated"
	StagePaying           Stage = "paying"
	StageNotifying        Stage = "notifying"
	StageDone             Stage = "done"
)

// Status is returned by StatusQuery.
type Status struct {
	Stage    Stage
	Decision *Decision `json:",omitempty"`
}

// ApprovalNotice asks an approver to review a request.
type ApprovalNotice struct {
	To        string
	Request   ExpenseRequest
	Escalated bool
}

// EmployeeNotice tells the employee how their request ended.
type EmployeeNotice struct {
	Request ExpenseRequest
	Result  ExpenseResult
}
