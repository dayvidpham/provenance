package provenance

import "github.com/dayvidpham/provenance/internal/journal"

// Assignment-start queries are optional on Journal; SQLite and borrowed SQLite
// support this interface. See docs/assignment-start-queries.md for paging and
// integrity boundaries.
type AssignmentStartQueryAPI = journal.AssignmentStartQueryAPI
type AssignmentStartPageRequest = journal.AssignmentStartPageRequest
type AssignmentStartQuery = journal.AssignmentStartQuery
type AssignmentStartRow = journal.AssignmentStartRow
type AssignmentStartPage = journal.AssignmentStartPage
type AssignmentStartCursor = journal.AssignmentStartCursor

const MaxAssignmentStartSlotFilters = journal.MaxAssignmentStartSlotFilters
