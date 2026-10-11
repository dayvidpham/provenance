package namespace_test

import (
	"testing"

	"github.com/dayvidpham/provenance/pkg/namespace"
	"github.com/dayvidpham/provenance/pkg/ptypes"
	"github.com/google/uuid"
)

func TestDirectoryIdentityURI(t *testing.T) {
	const suffix = "01968a3c-9d4f-7c8a-bc12-feedfacecafe"
	cases := []struct{ directory, wantNamespace, wantID string }{
		{"/home/user/project", "file:///home/user/project", "file:///home/user/project/" + suffix},
		{"/home/user/my--project/nested/", "file:///home/user/my--project/nested", "file:///home/user/my--project/nested/" + suffix},
		{"/", "file:///", "file:////" + suffix},
	}
	for _, tc := range cases {
		t.Run(tc.directory, func(t *testing.T) {
			ns := namespace.FromDirectory(tc.directory)
			if ns != tc.wantNamespace {
				t.Fatalf("FromDirectory(%q) = %q, want %q", tc.directory, ns, tc.wantNamespace)
			}
			id := ptypes.TaskID{Namespace: ns, UUID: uuid.MustParse(suffix)}
			if id.String() != tc.wantID {
				t.Fatalf("ID = %q, want %q", id.String(), tc.wantID)
			}
			got, err := ptypes.ParseTaskID(tc.wantID)
			if err != nil || got != id {
				t.Fatalf("ParseTaskID(%q) = %v, %v", tc.wantID, got, err)
			}
		})
	}
}
