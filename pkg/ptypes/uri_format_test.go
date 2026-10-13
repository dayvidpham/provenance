package ptypes_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func TestURIIdentityContract(t *testing.T) {
	parsers := []struct {
		name  string
		parse func(string) (string, string, uuid.UUID, error)
	}{
		{"ParseTaskID", func(s string) (string, string, uuid.UUID, error) {
			id, err := ptypes.ParseTaskID(s)
			return id.String(), id.Namespace, id.UUID, err
		}},
		{"ParseActorID", func(s string) (string, string, uuid.UUID, error) {
			id, err := ptypes.ParseActorID(s)
			return id.String(), id.Namespace, id.UUID, err
		}},
		{"ParseAgentID", func(s string) (string, string, uuid.UUID, error) {
			id, err := ptypes.ParseAgentID(s)
			return id.String(), id.Namespace, id.UUID, err
		}},
		{"ParseActivityID", func(s string) (string, string, uuid.UUID, error) {
			id, err := ptypes.ParseActivityID(s)
			return id.String(), id.Namespace, id.UUID, err
		}},
		{"ParseCommentID", func(s string) (string, string, uuid.UUID, error) {
			id, err := ptypes.ParseCommentID(s)
			return id.String(), id.Namespace, id.UUID, err
		}},
	}
	namespaces := []string{"project", "https://github.com/acme/my--repo", "a/b/c", "explicit/", "file:///home/user/my--project/nested", "file:///"}
	uuids := []string{"01968a3c-9d4f-7c8a-bc12-feedfacecafe", "00000000-0000-0000-0000-000000000000", "F47AC10B-58CC-4372-A567-0E02B2C3D479"}
	for _, parser := range parsers {
		t.Run(parser.name, func(t *testing.T) {
			for _, ns := range namespaces {
				for _, suffix := range uuids {
					raw := ns + "/" + suffix
					got, namespace, id, err := parser.parse(raw)
					if err != nil || namespace != ns || id != uuid.MustParse(suffix) || got != ns+"/"+strings.ToLower(suffix) {
						t.Fatalf("parse(%q) = (%q, %q, %v, %v)", raw, got, namespace, id, err)
					}
				}
			}
			invalid := []string{"", "project", "/" + uuids[0], "project/", "project/not-a-uuid", "project/01968a3c9d4f7c8abc12feedfacecafe", "project/{" + uuids[0] + "}", "project/urn:uuid:" + uuids[0], "project/" + uuids[0] + "?q=x", "project/" + uuids[0] + "#fragment", "project/" + uuids[0] + "/"}
			for _, ns := range namespaces {
				invalid = append(invalid, ns+"--"+uuids[0])
			}
			for _, raw := range invalid {
				_, namespace, id, err := parser.parse(raw)
				if !errors.Is(err, ptypes.ErrInvalidID) || namespace != "" || id != uuid.Nil {
					t.Fatalf("parse(%q) should reject without identity: ns=%q id=%v err=%v", raw, namespace, id, err)
				}
				for _, token := range []string{parser.name, "namespace>/<uuid", "before", "no identity", "fix:"} {
					if !strings.Contains(err.Error(), token) {
						t.Errorf("parse(%q) error lacks %q: %v", raw, token, err)
					}
				}
				if strings.Contains(raw, "--"+uuids[0]) && !strings.Contains(err.Error(), "retired two-dash") {
					t.Errorf("retired input needs diagnostic: %v", err)
				}
			}
		})
	}
}
