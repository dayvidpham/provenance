package sqlite

type taskEventQueryOrder uint8

const (
	taskEventQueryJournalOrder taskEventQueryOrder = iota + 1
	taskEventQueryTimelineOrder
)

func (order taskEventQueryOrder) statement() sealedSQLStatement {
	switch order {
	case taskEventQueryJournalOrder:
		return journalSelectJournalAttributed1222
	case taskEventQueryTimelineOrder:
		return journalSelectJournalAttributedfe94
	default:
		panic("unknown task-event query order")
	}
}
