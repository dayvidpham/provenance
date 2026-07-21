package provenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/pkg/ptypes"
)

type dbosFixtureField struct{ name, value string }

func immutableMutationFixture(fields []dbosFixtureField) []byte {
	var out bytes.Buffer
	write := func(name, value string) { fmt.Fprintf(&out, "%s:%d:%s\n", name, len(value), value) }
	write("version", "provenance.mutation.v1")
	write("effect-count", "1")
	for _, field := range fields {
		write("effect.0."+field.name, field.value)
	}
	return out.Bytes()
}

func immutableContextFixture(operation, actor string, authority *journal.JournalID, command []byte, recordedAt int64) []byte {
	fields := [][]byte{[]byte("provenance.dbos-context/v2"), []byte(operation), []byte(actor)}
	if authority == nil {
		fields = append(fields, []byte{0})
	} else {
		field := make([]byte, 9)
		field[0] = 1
		binary.BigEndian.PutUint64(field[1:], uint64(*authority))
		fields = append(fields, field)
	}
	fields = append(fields, append([]byte(nil), command...))
	timestamp := make([]byte, 8)
	binary.BigEndian.PutUint64(timestamp, uint64(recordedAt))
	fields = append(fields, timestamp)
	var out bytes.Buffer
	for _, field := range fields {
		_ = binary.Write(&out, binary.BigEndian, uint32(len(field)))
		out.Write(field)
	}
	return out.Bytes()
}

func immutableDBOSFamilyFixtures(t *testing.T) ([]journal.Effect, [][]dbosFixtureField) {
	t.Helper()
	task, _ := ptypes.ParseTaskID("fixture--018f0000-0000-7000-8000-000000000001")
	actor, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000002")
	comment, _ := ptypes.ParseCommentID("fixture--018f0000-0000-7000-8000-000000000003")
	title, description, notes := "new", "new description", "notes"
	priority, phase, recorded := ptypes.PriorityHigh, ptypes.PhaseCodeReview, int64(42)
	ctx, _ := journal.TaskContext(task)
	effects := []journal.Effect{
		{Sort: journal.EffectTaskEvent, ResultSlot: "event", RecordedAtOverride: &recorded, TaskID: task, EventKind: journal.EventKindTaskUpdated, Payload: []byte(`{"a":1,"b":2}`), Contexts: []journal.EventContext{ctx}, UpdateTitle: &title, UpdateDescription: &description, UpdatePriority: &priority, UpdatePhase: &phase, UpdateNotes: &notes},
		{Sort: journal.EffectTaskEvent, ResultSlot: "close", RecordedAtOverride: &recorded, TaskID: task, EventKind: journal.EventKindTaskClosed, CloseReason: "done", Forced: true},
		{Sort: journal.EffectBootstrapAuthority, ResultSlot: "bootstrap", RecordedAtOverride: &recorded, BootstrapLabel: "root", OperationAuthorityID: "auth"},
		{Sort: journal.EffectAssignmentStart, ResultSlot: "start", RecordedAtOverride: &recorded, TaskID: task, AssignmentID: "a", SlotID: journal.SlotOwnerResponsibility, Occupant: actor, Predecessor: "p", Parent: "parent"},
		{Sort: journal.EffectAssignmentEnd, ResultSlot: "end", RecordedAtOverride: &recorded, TaskID: task, AssignmentID: "a", SlotID: journal.SlotOwnerResponsibility},
		{Sort: journal.EffectDecision, ResultSlot: "decision", RecordedAtOverride: &recorded, TaskID: task, DecisionKind: "fixture.decision", Payload: []byte(`{"x":1}`)},
		{Sort: journal.EffectEvidence, ResultSlot: "evidence", RecordedAtOverride: &recorded, TaskID: task, EvidenceKind: "fixture.evidence", ContentDigest: []byte{1, 2}, Payload: []byte(`{"x":1}`)},
		{Sort: journal.EffectTaskCreate, ResultSlot: "create", RecordedAtOverride: &recorded, TaskID: task, Title: "title", Description: "description", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped, Payload: []byte(`{"x":1}`), Contexts: []journal.EventContext{ctx}},
		{Sort: journal.EffectTaskCreateAllocated, ResultSlot: "allocated", RecordedAtOverride: &recorded, TaskID: task, Title: "title", Description: "description", Type: ptypes.TaskTypeTask, Priority: ptypes.PriorityMedium, Phase: ptypes.PhaseUnscoped, Payload: []byte(`{"x":1}`), Contexts: []journal.EventContext{ctx}},
		{Sort: journal.EffectEdgeAdd, ResultSlot: "edge-add", RecordedAtOverride: &recorded, TaskID: task, EdgeTargetID: task.String(), EdgeRelKind: ptypes.EdgeDerivedFrom, Contexts: []journal.EventContext{ctx}},
		{Sort: journal.EffectEdgeRemove, ResultSlot: "edge-remove", RecordedAtOverride: &recorded, TaskID: task, EdgeTargetID: task.String(), EdgeRelKind: ptypes.EdgeDerivedFrom, Contexts: []journal.EventContext{ctx}},
		{Sort: journal.EffectLabelAdd, ResultSlot: "label-add", RecordedAtOverride: &recorded, TaskID: task, Label: "label", Contexts: []journal.EventContext{ctx}},
		{Sort: journal.EffectLabelRemove, ResultSlot: "label-remove", RecordedAtOverride: &recorded, TaskID: task, Label: "label", Contexts: []journal.EventContext{ctx}},
		{Sort: journal.EffectCommentAdd, ResultSlot: "comment", RecordedAtOverride: &recorded, TaskID: task, CommentIdentity: comment, CommentAuthor: actor, CommentBody: "body", Contexts: []journal.EventContext{ctx}},
	}
	common := func(family, slot string, rest ...dbosFixtureField) []dbosFixtureField {
		return append([]dbosFixtureField{{"family", family}, {"result-slot", slot}, {"recorded-at-override", "142"}}, rest...)
	}
	contexts := []dbosFixtureField{{"context-count", "1"}, {"context.0.kind", "task"}, {"context.0.identity", task.String()}}
	withContexts := func(fields []dbosFixtureField) []dbosFixtureField { return append(fields, contexts...) }
	fields := [][]dbosFixtureField{
		append(append(common("task_event", "event", dbosFixtureField{"task", task.String()}, dbosFixtureField{"event-kind", string(journal.EventKindTaskUpdated)}, dbosFixtureField{"payload", `{"a":1,"b":2}`}), contexts...), dbosFixtureField{"update-title", "1new"}, dbosFixtureField{"update-description", "1new description"}, dbosFixtureField{"update-priority", "1high"}, dbosFixtureField{"update-phase", "1code_review"}, dbosFixtureField{"update-notes", "1notes"}),
		common("task_event", "close", dbosFixtureField{"task", task.String()}, dbosFixtureField{"event-kind", string(journal.EventKindTaskClosed)}, dbosFixtureField{"payload", "{}"}, dbosFixtureField{"context-count", "0"}, dbosFixtureField{"forced", "true"}, dbosFixtureField{"close-reason", "done"}),
		common("bootstrap_authority", "bootstrap", dbosFixtureField{"bootstrap-label", "root"}, dbosFixtureField{"operation-authority", "auth"}),
		common("assignment_start", "start", dbosFixtureField{"task", task.String()}, dbosFixtureField{"assignment", "a"}, dbosFixtureField{"slot", "owner-responsibility"}, dbosFixtureField{"occupant", actor.String()}, dbosFixtureField{"predecessor", "p"}, dbosFixtureField{"parent", "parent"}),
		common("assignment_end", "end", dbosFixtureField{"task", task.String()}, dbosFixtureField{"assignment", "a"}, dbosFixtureField{"slot", "owner-responsibility"}),
		common("decision", "decision", dbosFixtureField{"task", task.String()}, dbosFixtureField{"decision-kind", "fixture.decision"}, dbosFixtureField{"payload", `{"x":1}`}),
		common("evidence", "evidence", dbosFixtureField{"task", task.String()}, dbosFixtureField{"evidence-kind", "fixture.evidence"}, dbosFixtureField{"content-digest", string([]byte{1, 2})}, dbosFixtureField{"payload", `{"x":1}`}),
		append(append(common("task_create", "create", dbosFixtureField{"task", task.String()}, dbosFixtureField{"payload", `{"x":1}`}), contexts...), dbosFixtureField{"title", "title"}, dbosFixtureField{"description", "description"}, dbosFixtureField{"type", "task"}, dbosFixtureField{"priority", "medium"}, dbosFixtureField{"phase", "unscoped"}),
		append(append(common("task_create_allocated", "allocated", dbosFixtureField{"task", task.String()}, dbosFixtureField{"payload", `{"x":1}`}), contexts...), dbosFixtureField{"title", "title"}, dbosFixtureField{"description", "description"}, dbosFixtureField{"type", "task"}, dbosFixtureField{"priority", "medium"}, dbosFixtureField{"phase", "unscoped"}),
		withContexts(common("edge_add", "edge-add", dbosFixtureField{"task", task.String()}, dbosFixtureField{"edge-target", task.String()}, dbosFixtureField{"edge-kind", "derived_from"})),
		withContexts(common("edge_remove", "edge-remove", dbosFixtureField{"task", task.String()}, dbosFixtureField{"edge-target", task.String()}, dbosFixtureField{"edge-kind", "derived_from"})),
		withContexts(common("label_add", "label-add", dbosFixtureField{"task", task.String()}, dbosFixtureField{"label", "label"})),
		withContexts(common("label_remove", "label-remove", dbosFixtureField{"task", task.String()}, dbosFixtureField{"label", "label"})),
		withContexts(common("comment_add", "comment", dbosFixtureField{"task", task.String()}, dbosFixtureField{"comment", comment.String()}, dbosFixtureField{"comment-author", actor.String()}, dbosFixtureField{"comment-body", "body"})),
	}
	return effects, fields
}

func TestDBOSV2IndependentImmutableWireFixturesEveryFamily(t *testing.T) {
	effects, fixtures := immutableDBOSFamilyFixtures(t)
	closedSorts := make(map[journal.EffectSort]struct{})
	for _, effect := range effects {
		closedSorts[effect.Sort] = struct{}{}
	}
	if len(closedSorts) != 13 {
		t.Fatalf("immutable DBOS fixture corpus covers %d distinct EffectSort values, want closed set of 13", len(closedSorts))
	}
	actor, _ := ptypes.ParseActorID("fixture--018f0000-0000-7000-8000-000000000002")
	authority := journal.JournalID(7)
	context := immutableContextFixture("fixture-operation", actor.String(), &authority, []byte("fixture-command"), 99)
	for i := range fixtures {
		t.Run(effects[i].Sort.String()+fmt.Sprintf("-%d", i), func(t *testing.T) {
			mutation := immutableMutationFixture(fixtures[i])
			decoded, err := decodeApplyInputV2(DBOSApplyInputV2{Schema: "provenance.dbos-apply-input/v2", Context: context, Mutation: mutation})
			if err != nil {
				t.Fatalf("decode immutable fixture: %v\nwire=%q", err, mutation)
			}
			if !reflect.DeepEqual(decoded.Effects, []journal.Effect{effects[i]}) {
				t.Fatalf("decoded semantics=%#v want independently authored %#v", decoded.Effects, []journal.Effect{effects[i]})
			}
			digest := sha256.Sum256(mutation)
			if !bytes.Equal(decoded.MutationDigest, digest[:]) || decoded.RecordedAt != 99 || decoded.OperationID != "fixture-operation" || decoded.ActorID != actor || decoded.AuthorityJournalID == nil || *decoded.AuthorityJournalID != authority {
				t.Fatalf("decoded V2 envelope drifted: %#v", decoded)
			}
		})
	}
}

func TestDBOSV2IndependentStrictNegativeFixturesAndBounds(t *testing.T) {
	_, fixtures := immutableDBOSFamilyFixtures(t)
	actor := "fixture--018f0000-0000-7000-8000-000000000002"
	authority := journal.JournalID(7)
	context := immutableContextFixture("fixture-operation", actor, &authority, []byte("fixture-command"), 99)
	mutation := immutableMutationFixture(fixtures[0])
	cases := map[string]DBOSApplyInputV2{
		"unknown-version":      {Schema: DBOSApplyInputSchemaV2, Context: context, Mutation: bytes.Replace(mutation, []byte("provenance.mutation.v1"), []byte("provenance.mutation.v2"), 1)},
		"unknown-field":        {Schema: DBOSApplyInputSchemaV2, Context: context, Mutation: bytes.Replace(mutation, []byte("effect.0.task"), []byte("effect.0.zzzz"), 1)},
		"missing-field":        {Schema: DBOSApplyInputSchemaV2, Context: context, Mutation: bytes.Replace(mutation, []byte("effect.0.result-slot"), []byte("missing.result.slot"), 1)},
		"duplicate-field":      {Schema: DBOSApplyInputSchemaV2, Context: context, Mutation: bytes.Replace(mutation, []byte("effect.0.recorded-at-override"), []byte("effect.0.result-slot          "), 1)},
		"trailing-field":       {Schema: DBOSApplyInputSchemaV2, Context: context, Mutation: append(append([]byte(nil), mutation...), 'x')},
		"context-duplicate":    {Schema: DBOSApplyInputSchemaV2, Context: append(append([]byte(nil), context...), 0, 0, 0, 0), Mutation: mutation},
		"context-out-of-order": {Schema: DBOSApplyInputSchemaV2, Context: immutableContextFixture(actor, "fixture-operation", &authority, []byte("fixture-command"), 99), Mutation: mutation},
		"context-version":      {Schema: DBOSApplyInputSchemaV2, Context: immutableContextFixture("fixture-operation", actor, &authority, []byte("fixture-command"), 99), Mutation: mutation},
	}
	// Mutate the literal context version without invoking the production codec.
	cv := cases["context-version"]
	cv.Context = bytes.Replace(cv.Context, []byte("provenance.dbos-context/v2"), []byte("provenance.dbos-context/v3"), 1)
	cases["context-version"] = cv
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeApplyInputV2(input); err == nil {
				t.Fatal("strict negative fixture decoded")
			}
		})
	}
	exact := journal.OperationInput{OperationID: journal.OperationID(strings.Repeat("x", MaxCanonicalFieldBytes)), ActorID: testActorID(t), CommandDigest: bytes.Repeat([]byte{'c'}, MaxCanonicalFieldBytes)}
	if _, err := encodeDBOSContextV2(exact); err != nil {
		t.Fatalf("exact V2 field bounds rejected: %v", err)
	}
	over := exact
	over.CommandDigest = append(over.CommandDigest, 'x')
	if _, err := encodeDBOSContextV2(over); err == nil {
		t.Fatal("V2 field over bound accepted")
	}
	oversized := DBOSApplyInputV2{Schema: DBOSApplyInputSchemaV2, Context: context, Mutation: bytes.Repeat([]byte{'x'}, MaxCanonicalMutationBytes+1)}
	if _, err := decodeApplyInputV2(oversized); err == nil {
		t.Fatal("V2 mutation over bound accepted")
	}
}
