# Baseline timing inventory - plain suite (commit 8bf1a9b)

## Per-package elapsed

| package | elapsed |
|---|---:|
| github.com/dayvidpham/provenance | 77.92s |
| github.com/dayvidpham/provenance/internal/sqlite | 12.44s |
| github.com/dayvidpham/provenance/internal/fusedtx | 2.55s |
| github.com/dayvidpham/provenance/internal/journal | 0.25s |
| github.com/dayvidpham/provenance/internal/helpers | 0.23s |
| github.com/dayvidpham/provenance/internal/graph | 0.12s |
| github.com/dayvidpham/provenance/internal/allocation | 0.05s |
| github.com/dayvidpham/provenance/pkg/ptypes | 0.01s |
| github.com/dayvidpham/provenance/pkg/namespace | 0.01s |
| github.com/dayvidpham/provenance/internal/testcorpus | 0.00s |

## Root package: serial vs parallel top-level tests

- serial tests: 133, sum elapsed **68.27s** (critical-path floor)
- parallel tests: 165, sum elapsed 177.83s
- package wall: 77.92s

## Serial-phase cost by file

| file | tests | elapsed |
|---|---:|---:|
| governed_allocation_integration_test.go | 23 | 29.11s |
| dbos_assignment_transfer_test.go | 3 | 4.25s |
| dbos_matrix_test.go | 1 | 4.19s |
| governed_allocation_failure_arm_integrity_test.go | 5 | 4.10s |
| governed_allocation_composed_batch_test.go | 6 | 3.23s |
| governed_allocation_identity_replay_test.go | 5 | 2.61s |
| sql_architecture_test.go | 4 | 2.46s |
| dbos_apply_parity_test.go | 3 | 2.03s |
| governed_allocation_bound_runtime_test.go | 3 | 1.75s |
| governed_allocation_depth_test.go | 2 | 1.60s |
| governed_allocation_receipt_integrity_test.go | 2 | 1.21s |
| governed_allocation_reducer_parity_test.go | 2 | 1.16s |
| governed_allocation_shape_conflict_test.go | 2 | 1.12s |
| governed_allocation_composed_contract_test.go | 3 | 1.07s |
| dbos_recovery_parity_test.go | 1 | 1.05s |
| dbos_cancel_test.go | 2 | 1.04s |
| governed_allocation_fused_legacy_replay_test.go | 1 | 1.02s |
| assignment_transfer_test.go | 7 | 1.01s |
| governed_allocation_composed_failure_test.go | 1 | 0.60s |
| dbos_config_test.go | 3 | 0.55s |
| governed_allocation_atomic_rollback_test.go | 1 | 0.54s |
| governed_allocation_forced_replay_test.go | 1 | 0.52s |
| dbos_durable_deep_test.go | 1 | 0.50s |
| dbos_family_retry_test.go | 3 | 0.48s |
| hygiene_test.go | 3 | 0.48s |
| governed_allocation_authority_boundary_test.go | 1 | 0.18s |
| dbos_harness_test.go | 1 | 0.12s |
| dbos_compilefail_test.go | 1 | 0.06s |
| journal_watermark_test.go | 1 | 0.04s |
| ast_grep_rules_test.go | 2 | 0.03s |
| dbos_exhaustive_retry_test.go | 1 | 0.03s |
| dbos_failure_contract_test.go | 5 | 0.03s |
| create_permutation_test.go | 1 | 0.03s |
| journal_contract_compile_test.go | 1 | 0.03s |
| tracker_test.go | 1 | 0.03s |
| dbos_contract_fixture_test.go | 5 | 0.01s |
| canonical_allocation_proof_test.go | 1 | 0.00s |
| dbos_crashgap_test.go | 1 | 0.00s |
| dbos_internal_test.go | 10 | 0.00s |
| error_message_oracle_test.go | 2 | 0.00s |
| journal_authority_revocation_race_test.go | 1 | 0.00s |
| journal_concurrent_writers_test.go | 4 | 0.00s |
| journal_corpus_test.go | 1 | 0.00s |
| adapter_test.go | 2 | 0.00s |
| reexport_test.go | 3 | 0.00s |

## DBOS context launches (total 161)

| launches | test |
|---:|---|
| 51 | TestFusedGovernedAllocationComposedRejectsStructurallyForgedSQLiteReceipts |
| 8 | TestMatrix_SuccessCheckpointRejectsResultSlotDivergence |
| 7 | TestCanonicalRetryMatrixSameProcessAndReopen |
| 6 | TestFusedGovernedAllocationRejectsForgedDBOSOutputAfterReopen |
| 6 | TestDBOSRetryYAMLValuesAreAuthoritative |
| 5 | TestCanonicalRetryMatrixSimultaneousIndependentHandles |
| 4 | TestGovernedAllocationCommittedSuccessCannotBeReplacedByValidFailureArm |
| 3 | TestDBOSAdapterTransferAssignmentSuccessAndReplayAcrossWorkflows |
| 3 | TestDBOSCompletedRetryEveryCanonicalOperandHasZeroCallbackAndWrites |
| 2 | TestDBOSRecoveredConditionAndActivityParity |
| 2 | TestFusedLegacyRunAllocateReopensBaselineWorkflowWithoutParticipant |
| 2 | TestBoundGovernedAllocatorReopenReplaySuppressesParticipant |
| 2 | TestFusedGovernedAllocationComposedBatchReopenReplayPreservesOrder |
| 2 | TestComposedActivityChronologyRejectsTamperingAcrossReplayAndReopen |
| 2 | TestComposedReceiptDetectsForeignProducerMutationOnCanonicalAndExtraRows |
| 2 | TestFusedGovernedAllocationComposedForcedTransitionsSurviveReopen |
| 2 | TestFusedGovernedAllocationComposedReducerAndParticipantFailuresRollBack |
| 2 | TestFusedGovernedAllocationComposedExactReopenReplayIsStableAndSkipsParticipant |
| 2 | TestCanonicalColumnPreflightErrorsAreTypedActionableAndReadOnly |
| 2 | TestContractCorpusExecutesImplementedPartitions |

## All top-level tests > 1s

| elapsed | mode | package | test |
|---:|---|---|---|
| 19.50s | serial | provenance | TestFusedGovernedAllocationComposedRejectsStructurallyForgedSQLiteReceipts |
| 6.25s | parallel | provenance | TestNewDBOSAdapter_VersionMismatch |
| 5.87s | parallel | provenance | TestStartupCorruptionMatrixLeavesBytesUnchanged |
| 5.75s | parallel | provenance | TestCanonicalColumnPreflightErrorsAreTypedActionableAndReadOnly |
| 5.71s | parallel | provenance | TestCanonicalRetryMatrixSameProcessAndReopen |
| 5.45s | parallel | provenance | TestDBOSCompletedRetryUsesOneValidBaselineAndOneFieldChangePerFamily |
| 5.35s | parallel | provenance | TestMatrix_FailureCheckpointRejectsCommittedJournal |
| 5.14s | parallel | provenance | TestMixedLegacyCanonicalMalformedPairsFailWithoutByteDrift |
| 4.63s | parallel | provenance | TestDBOSRegisteredBorrowedJournalRetryAndTerminalSemantics |
| 4.61s | parallel | provenance | TestCanonicalRetryMatrixSimultaneousIndependentHandles |
| 4.21s | parallel | provenance | TestAllocatedCreateCorruptionFailsLiveAndOnOpenWithoutDrift |
| 4.19s | serial | provenance | TestMatrix_SuccessCheckpointRejectsResultSlotDivergence |
| 3.70s | parallel | provenance | TestAllocatedCreateApplyReturnsCompleteResultAcrossRetryModes |
| 3.63s | parallel | provenance | TestCanonicalSchemaMigrationAndMixedLegacyRowsAreIdempotent |
| 3.19s | parallel | provenance | TestMissingJournalOperationFKMigrationIsComposableAndIdempotent |
| 3.17s | parallel | provenance | TestPinnedSessionCreateSameProcessAndReopenUUIDv7 |
| 3.13s | - | provenance/internal/sqlite | TestFactQueriesCorruptionMatrixFailsClosedForBothSubtypes |
| 2.95s | parallel | provenance | TestDBOSCompletedRetryEveryCanonicalOperandHasZeroCallbackAndWrites |
| 2.53s | parallel | provenance | TestDemo_Persistence |
| 2.28s | parallel | provenance | TestV1SpecificSQLAuthorityMigratesOnce |
| 2.14s | parallel | provenance | TestCancel_DeadlineWhileGated |
| 2.11s | serial | provenance | TestDBOSAdapterTransferAssignmentRevocationRaceParity |
| 2.08s | parallel | provenance | TestCrashGap2_StepCheckpointBeforeCompletion |
| 2.08s | parallel | provenance | TestDBOSBorrowedSQLitePool16SupportsConcurrentOperations |
| 2.01s | parallel | provenance | TestCrashGap0_BeforeDomainCommit |
| 2.00s | parallel | provenance | TestCrashGap1_DomainCommitBeforeCheckpoint |
| 1.96s | parallel | provenance | TestDeleteModeCorruptionPreflightIsByteAndModeReadOnly |
| 1.94s | parallel | provenance | TestConcurrentCreate |
| 1.94s | parallel | provenance | TestOpenBorrowedSQLite_PreservesCallerPoolLimits |
| 1.88s | parallel | provenance | TestDeleteModeActivationSchemaFailureDoesNotPersistWAL |
| 1.85s | parallel | provenance | TestRegisterFixedSoftwareAgentRollsBackEveryInsertBoundary |
| 1.80s | parallel | provenance | TestPinnedSessionCreateAcrossIndependentHandles |
| 1.74s | serial | provenance | TestFusedGovernedAllocationRejectsForgedDBOSOutputAfterReopen |
| 1.74s | parallel | provenance | TestMatrix_AbsentExact_ReplaySucceeds |
| 1.73s | parallel | provenance | TestMatrix_PresentSuccessChangedCanonicalOperand_ConflictsBeforeWorkflow |
| 1.73s | parallel | provenance | TestDBOSSimultaneousExactAndChangedMutationRaces |
| 1.68s | parallel | provenance | TestMatrix_PresentSuccessExact_ZeroCallback |
| 1.68s | parallel | provenance | TestMatrix_AbsentAbsent_Succeeds |
| 1.67s | parallel | provenance | TestJournalFactsPublicFileBackedDecisionEvidenceSurviveReopen |
| 1.65s | parallel | provenance | TestMatrix_PresentSuccessAbsent_Divergence |
| 1.65s | parallel | provenance | TestMatrix_TimestampOnlyRetryAttachesCompletedWorkflow |
| 1.64s | parallel | provenance | TestCanonicalRetryAcrossIndependentHandles |
| 1.62s | parallel | provenance | TestMatrix_AbsentConflict_TypedConflict |
| 1.61s | parallel | provenance | TestDemo_MultiProviderAgentsFromBestiary |
| 1.61s | parallel | provenance | TestMatrix_PresentSuccessMismatch_Divergence |
| 1.61s | parallel | provenance | TestMatrix_AllocatedCreateChangedProvisionalUUIDReturnsOriginal |
| 1.60s | parallel | provenance | TestRegisterFixedSoftwareAgentErrorsAreActionable |
| 1.59s | parallel | provenance | TestActivityCreate_ReopenReconstructsActivitySlot |
| 1.59s | parallel | provenance | TestRegisterFixedSoftwareAgentConcurrentStartup |
| 1.58s | parallel | provenance | TestDBOSExplicitResponseLossRetrievesCompleteResult |
| 1.56s | parallel | provenance | TestDBOSSmoke_ApplyCreatesTask |
| 1.56s | parallel | provenance | TestMatrix_UnknownLookupVariant_FailClosed |
| 1.55s | parallel | provenance | TestBorrowed_ReadOnlyQueriesCreateNoEvent |
| 1.54s | parallel | provenance | TestMatrix_PresentFailureOutcome_TypedFailurePermanent |
| 1.53s | parallel | provenance | TestMatrix_PresentSuccessConflict_Divergence |
| 1.50s | serial | provenance | TestProductionSQLTextIsDecidableFromSource |
| 1.49s | parallel | provenance | TestRevokeVsInFlightCitationNoTOCTOU |
| 1.46s | serial | provenance | TestGovernedAllocationAcceptsDeepAcyclicAncestry |
| 1.44s | parallel | provenance | TestCanonicalExactRetryAfterReopenReturnsCompleteResult |
| 1.42s | parallel | provenance | TestCanonicalTaskStateSurvivesRestartAndReplay |
| 1.29s | serial | provenance | TestGovernedAllocationCommittedSuccessCannotBeReplacedByValidFailureArm |
| 1.18s | serial | provenance | TestDBOSAdapterTransferAssignmentSuccessAndReplayAcrossWorkflows |
| 1.13s | serial | provenance | TestComposedReceiptDetectsForeignProducerMutationOnCanonicalAndExtraRows |
| 1.09s | serial | provenance | TestFusedGovernedAllocationComposedReducerAndParticipantFailuresRollBack |
| 1.08s | parallel | provenance | TestRegisterFixedSoftwareAgentValidationCorpus |
| 1.05s | serial | provenance | TestDBOSActivityConflictIsCheckpointedTypedAndActivityResultTransports |
| 1.05s | serial | provenance | TestDBOSRecoveredConditionAndActivityParity |
| 1.02s | serial | provenance | TestFusedLegacyRunAllocateReopensBaselineWorkflowWithoutParticipant |
