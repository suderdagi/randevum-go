package booking

type Status string

const (
	Pending   Status = "pending"
	Confirmed Status = "confirmed"
	Cancelled Status = "cancelled"
	Completed Status = "completed"
)

type actor string

const (
	asOwner    actor = "owner"
	asCustomer actor = "customer"
)

// transitions lists every allowed status change and who may make it.
// Keeping the rules in one table makes them easy to read, test and change.
var transitions = map[actor]map[Status][]Status{
	asOwner: {
		Pending:   {Confirmed, Cancelled},
		Confirmed: {Cancelled, Completed},
	},
	asCustomer: {
		Pending:   {Cancelled},
		Confirmed: {Cancelled},
	},
}

func canTransition(a actor, from, to Status) bool {
	for _, allowed := range transitions[a][from] {
		if allowed == to {
			return true
		}
	}
	return false
}
