package sqlite

type taskEventQueryOrder uint8

const (
	taskEventQueryJournalOrder taskEventQueryOrder = iota + 1
	taskEventQueryTimelineOrder
)

func (order taskEventQueryOrder) statement() sqlStatement {
	switch order {
	case taskEventQueryJournalOrder:
		return sqlStatement263
	case taskEventQueryTimelineOrder:
		return sqlStatement264
	default:
		panic("unknown task-event query order")
	}
}
