package sqlite

import "github.com/dayvidpham/provenance/internal/journal"

// decodedTaskEvent contains only immutable, purely decoded journal data. Errors
// are stored until the corresponding point in the in-order reducer: attribution
// and canonical checks precede payload validation in the original reducer.
type decodedTaskEvent struct {
	task       journal.TaskID
	taskErr    error
	kind       journal.EventKind
	edge       journal.EdgeMutationPayload
	label      journal.LabelMutationPayload
	comment    journal.CommentMutationPayload
	payloadErr error
	legacy     journal.TaskStatus
	legacyOK   bool
	forced     bool
}

func decodeTaskEventRow(taskRaw, kindRaw string, payload []byte) decodedTaskEvent {
	event := decodedTaskEvent{kind: journal.EventKind(kindRaw)}
	event.task, event.taskErr = journalParseTask(taskRaw)
	switch event.kind {
	case journal.EventKindEdgeAdded, journal.EventKindEdgeRemoved:
		event.edge, event.payloadErr = journal.DecodeEdgeMutationPayload(payload)
	case journal.EventKindLabelAdded, journal.EventKindLabelRemoved:
		event.label, event.payloadErr = journal.DecodeLabelMutationPayload(payload)
	case journal.EventKindCommentAdded:
		event.comment, event.payloadErr = journal.DecodeCommentMutationPayload(payload)
	case journal.EventKindTaskMigrated:
		event.legacy, event.legacyOK, event.payloadErr = journal.DecodeLegacyStatus(payload)
	default:
		if journal.IsTransitionLifecycleKind(event.kind) {
			event.forced, event.payloadErr = journal.DecodeForcedTransition(payload)
		}
	}
	return event
}
