package provenance

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dayvidpham/provenance/internal/journal"
	"github.com/dayvidpham/provenance/internal/sqlite"
)

type IDConversionReport = sqlite.IDConversionReport

// IDConversionOptions lets external typed owners translate their declared
// fields. Callbacks must not begin/commit transactions, read a second connection,
// rewrite prose, or guess digests. Native host bindings remain opaque evidence.
type IDConversionOptions struct {
	RewriteEffect func(Effect) (Effect, error)
	// RewriteContext translates extension-kind SQL context rows. For contexts
	// backed by a canonical effect, RewriteEffect must translate the identical
	// context too. Canonical condition extension contexts have no rewrite hook:
	// they must remain identity-equivalent, otherwise verification fails closed.
	RewriteContext func(string, string) (string, error)
}

func ConvertIDsInTx(ctx context.Context, tx *sql.Tx, options IDConversionOptions) (IDConversionReport, error) {
	return sqlite.ConvertIDsInTx(ctx, tx, sqlite.IDConversionOptions{RewriteEffect: options.RewriteEffect, RewriteContext: options.RewriteContext, RewriteCommand: migrateSessionCommandDigest})
}

func VerifyIDConversionInTx(ctx context.Context, tx *sql.Tx) error {
	return sqlite.VerifyIDConversionInTx(ctx, tx)
}

// PublishIDFormatInTx is called LAST, after external owners have verified their
// projections and the full raw-evidence inventory. It owns no transaction end.
func PublishIDFormatInTx(ctx context.Context, tx *sql.Tx) error {
	return sqlite.PublishIDFormatInTx(ctx, tx)
}

// Session.resolve is the command owner. Match its exact old recipe before
// deriving its new digest; an unmatched caller-supplied digest is opaque and
// retained. No table-column spelling or generic migration hash defines identity.
func migrateSessionCommandDigest(oldActor, newActor string, stored []byte, old, new journal.CanonicalMutation) ([]byte, error) {
	oldEffects, newEffects := old.NormalizedEffects(), new.NormalizedEffects()
	if len(oldEffects) != len(newEffects) {
		return nil, fmt.Errorf("Session command migration: effect count changed; no digest returned; fix: preserve operation effect order/count")
	}
	oldAtomic := fmt.Sprintf("n=%d", len(oldEffects))
	for i, e := range oldEffects {
		oldAtomic += fmt.Sprintf("|%d:%s:%s:%s:%s", i, e.Sort, retiredTaskString(e.TaskID), string(e.EventKind), string(e.AssignmentID))
	}
	candidates := [][2][]string{{{"atomic", oldAtomic}, {"atomic", atomicCanonical(newEffects)}}}
	if len(oldEffects) == 2 && oldEffects[0].Sort == EffectAssignmentEnd && oldEffects[1].Sort == EffectAssignmentStart {
		a, b := oldEffects, newEffects
		candidates = append(candidates, [2][]string{{"assignment-transfer", retiredTaskString(a[1].TaskID), string(a[0].SlotID), string(a[0].AssignmentID), string(a[1].AssignmentID), a[1].Occupant.Namespace + "--" + a[1].Occupant.UUID.String()}, {"assignment-transfer", b[1].TaskID.String(), string(b[0].SlotID), string(b[0].AssignmentID), string(b[1].AssignmentID), b[1].Occupant.String()}})
	}
	if len(oldEffects) == 1 {
		a, b := oldEffects[0], newEffects[0]
		switch a.Sort {
		case EffectTaskCreate, EffectTaskCreateAllocated:
			candidates = append(candidates, [2][]string{{"create", a.TaskID.Namespace, a.Title, a.Description, fmt.Sprintf("%d/%d/%d", int(a.Type), int(a.Priority), int(a.Phase))}, {"create", b.TaskID.Namespace, b.Title, b.Description, fmt.Sprintf("%d/%d/%d", int(b.Type), int(b.Priority), int(b.Phase))}})
		case EffectTaskEvent:
			if a.EventKind == EventKindTaskUpdated {
				fields := func(e Effect) UpdateFields {
					return UpdateFields{Title: e.UpdateTitle, Description: e.UpdateDescription, Priority: e.UpdatePriority, Phase: e.UpdatePhase, Notes: e.UpdateNotes}
				}
				candidates = append(candidates, [2][]string{{"update", retiredTaskString(a.TaskID), updateCanonical(fields(a))}, {"update", b.TaskID.String(), updateCanonical(fields(b))}})
			} else {
				candidates = append(candidates, [2][]string{{"lifecycle", retiredTaskString(a.TaskID), string(a.EventKind), a.CloseReason, fmt.Sprintf("forced=%t", a.Forced)}, {"lifecycle", b.TaskID.String(), string(b.EventKind), b.CloseReason, fmt.Sprintf("forced=%t", b.Forced)}})
			}
		case EffectEdgeAdd, EffectEdgeRemove:
			verb := "edge-add"
			if a.Sort == EffectEdgeRemove {
				verb = "edge-remove"
			}
			candidates = append(candidates, [2][]string{{verb, retiredTaskString(a.TaskID), retiredRenderedID(a.EdgeTargetID), fmt.Sprintf("k=%d", int(a.EdgeRelKind))}, {verb, b.TaskID.String(), b.EdgeTargetID, fmt.Sprintf("k=%d", int(b.EdgeRelKind))}})
		case EffectLabelAdd, EffectLabelRemove:
			verb := "label-add"
			if a.Sort == EffectLabelRemove {
				verb = "label-remove"
			}
			candidates = append(candidates, [2][]string{{verb, retiredTaskString(a.TaskID), a.Label}, {verb, b.TaskID.String(), b.Label}})
		case EffectCommentAdd:
			candidates = append(candidates, [2][]string{{"comment-add", retiredTaskString(a.TaskID), a.CommentIdentity.Namespace + "--" + a.CommentIdentity.UUID.String(), a.CommentAuthor.Namespace + "--" + a.CommentAuthor.UUID.String(), a.CommentBody}, {"comment-add", b.TaskID.String(), b.CommentIdentity.String(), b.CommentAuthor.String(), b.CommentBody}})
		}
	}
	for _, candidate := range candidates {
		if bytes.Equal(stored, digestOf(append([]string{"command", oldActor}, candidate[0]...)...)) {
			return digestOf(append([]string{"command", newActor}, candidate[1]...)...), nil
		}
	}
	return append([]byte(nil), stored...), nil
}

func retiredTaskString(id TaskID) string { return id.Namespace + "--" + id.UUID.String() }
func retiredRenderedID(raw string) string {
	i := strings.LastIndexByte(raw, '/')
	if i < 0 {
		return raw
	}
	return raw[:i] + "--" + raw[i+1:]
}
