# Race timing inventory after the forged-receipt parallel commit (19e29e6)

## Per-package elapsed

| package | elapsed |
|---|---:|
| github.com/dayvidpham/provenance | 289.17s |
| github.com/dayvidpham/provenance/internal/sqlite | 80.45s |
| github.com/dayvidpham/provenance/internal/helpers | 6.41s |
| github.com/dayvidpham/provenance/internal/graph | 3.55s |
| github.com/dayvidpham/provenance/internal/fusedtx | 3.09s |
| github.com/dayvidpham/provenance/internal/journal | 2.88s |
| github.com/dayvidpham/provenance/internal/allocation | 1.40s |
| github.com/dayvidpham/provenance/pkg/namespace | 1.03s |
| github.com/dayvidpham/provenance/pkg/ptypes | 1.03s |
| github.com/dayvidpham/provenance/internal/testcorpus | 1.01s |

## Root package: serial vs parallel top-level tests

- serial tests: 133, sum elapsed **201.25s** (critical-path floor)
- parallel tests: 165, sum elapsed 1536.54s
- package wall: 289.17s

## Serial-phase cost by file

| file | tests | elapsed |
|---|---:|---:|
| governed_allocation_integration_test.go | 23 | 47.35s |
| governed_allocation_failure_arm_integrity_test.go | 5 | 14.38s |
| dbos_matrix_test.go | 1 | 13.88s |
| governed_allocation_composed_batch_test.go | 6 | 11.85s |
| governed_allocation_depth_test.go | 2 | 10.86s |
| dbos_assignment_transfer_test.go | 3 | 10.02s |
| hygiene_test.go | 3 | 8.68s |
| governed_allocation_identity_replay_test.go | 5 | 8.64s |
| assignment_transfer_test.go | 7 | 8.26s |
| governed_allocation_bound_runtime_test.go | 3 | 7.73s |
| governed_allocation_reducer_parity_test.go | 2 | 7.67s |
| governed_allocation_authority_boundary_test.go | 1 | 5.09s |
| dbos_recovery_parity_test.go | 1 | 4.04s |
| governed_allocation_composed_contract_test.go | 3 | 3.65s |
| governed_allocation_receipt_integrity_test.go | 2 | 3.62s |
| governed_allocation_shape_conflict_test.go | 2 | 3.56s |
| dbos_apply_parity_test.go | 3 | 3.48s |
| dbos_cancel_test.go | 2 | 3.35s |
| governed_allocation_forced_replay_test.go | 1 | 3.08s |
| sql_architecture_test.go | 4 | 2.97s |
| governed_allocation_composed_failure_test.go | 1 | 2.84s |
| governed_allocation_fused_legacy_replay_test.go | 1 | 2.72s |
| governed_allocation_atomic_rollback_test.go | 1 | 1.93s |
| dbos_harness_test.go | 1 | 1.88s |
| dbos_durable_deep_test.go | 1 | 1.71s |
| dbos_family_retry_test.go | 3 | 1.66s |
| dbos_config_test.go | 3 | 1.55s |
| journal_watermark_test.go | 1 | 1.26s |
| journal_contract_compile_test.go | 1 | 1.04s |
| tracker_test.go | 1 | 1.00s |
| create_permutation_test.go | 1 | 0.75s |
| dbos_exhaustive_retry_test.go | 1 | 0.36s |
| dbos_failure_contract_test.go | 5 | 0.12s |
| dbos_contract_fixture_test.go | 5 | 0.09s |
| dbos_compilefail_test.go | 1 | 0.07s |
| reexport_test.go | 3 | 0.04s |
| journal_corpus_test.go | 1 | 0.04s |
| dbos_internal_test.go | 10 | 0.02s |
| ast_grep_rules_test.go | 2 | 0.01s |
| adapter_test.go | 2 | 0.00s |
| journal_concurrent_writers_test.go | 4 | 0.00s |
| dbos_crashgap_test.go | 1 | 0.00s |
| error_message_oracle_test.go | 2 | 0.00s |
| journal_authority_revocation_race_test.go | 1 | 0.00s |
| canonical_allocation_proof_test.go | 1 | 0.00s |

## DBOS context launches (total 161)

| launches | test |
|---:|---|
| 51 | TestFusedGovernedAllocationComposedRejectsStructurallyForgedSQLiteReceipts |
| 9 | TestDBOSRetryYAMLValuesAreAuthoritative |
| 8 | TestMatrix_SuccessCheckpointRejectsResultSlotDivergence |
| 6 | TestFusedGovernedAllocationRejectsForgedDBOSOutputAfterReopen |
| 4 | TestGovernedAllocationCommittedSuccessCannotBeReplacedByValidFailureArm |
| 3 | TestDBOSAdapterTransferAssignmentSuccessAndReplayAcrossWorkflows |
| 3 | TestBorrowed_MigrationsCoexist_FreshExistingRepeat |
| 3 | TestCanonicalRetryMatrixSameProcessAndReopen |
| 2 | TestFusedGovernedAllocationComposedBatchReopenReplayPreservesOrder |
| 2 | TestBoundGovernedAllocatorReopenReplaySuppressesParticipant |
| 2 | TestFusedLegacyRunAllocateReopensBaselineWorkflowWithoutParticipant |
| 2 | TestFusedGovernedAllocationComposedForcedTransitionsSurviveReopen |
| 2 | TestFusedGovernedAllocationComposedExactReopenReplayIsStableAndSkipsParticipant |
| 2 | TestComposedReceiptDetectsForeignProducerMutationOnCanonicalAndExtraRows |
| 2 | TestFusedGovernedAllocationComposedReducerAndParticipantFailuresRollBack |
| 2 | TestDBOSRecoveredConditionAndActivityParity |
| 2 | TestComposedActivityChronologyRejectsTamperingAcrossReplayAndReopen |
| 2 | TestDBOSCompletedRetryEveryCanonicalOperandHasZeroCallbackAndWrites |
| 2 | TestCreate_Permutations |
| 2 | TestContractCorpusExecutesImplementedPartitions |

## All top-level tests > 1s

| elapsed | mode | package | test |
|---:|---|---|---|
| 60.42s | parallel | provenance | TestCanonicalRetryMatrixSameProcessAndReopen |
| 41.48s | parallel | provenance | TestStartupCorruptionMatrixLeavesBytesUnchanged |
| 40.55s | parallel | provenance | TestCanonicalRetryMatrixSimultaneousIndependentHandles |
| 40.43s | parallel | provenance | TestAllocatedCreateApplyReturnsCompleteResultAcrossRetryModes |
| 39.08s | parallel | provenance | TestDBOSCompletedRetryUsesOneValidBaselineAndOneFieldChangePerFamily |
| 38.56s | parallel | provenance | TestCanonicalSchemaMigrationAndMixedLegacyRowsAreIdempotent |
| 38.23s | parallel | provenance | TestCanonicalColumnPreflightErrorsAreTypedActionableAndReadOnly |
| 37.22s | parallel | provenance | TestMissingJournalOperationFKMigrationIsComposableAndIdempotent |
| 36.93s | parallel | provenance | TestMixedLegacyCanonicalMalformedPairsFailWithoutByteDrift |
| 35.54s | parallel | provenance | TestAllocatedCreateCorruptionFailsLiveAndOnOpenWithoutDrift |
| 33.46s | parallel | provenance | TestPinnedSessionCreateSameProcessAndReopenUUIDv7 |
| 28.72s | parallel | provenance | TestDemo_Persistence |
| 27.12s | parallel | provenance | TestV1SpecificSQLAuthorityMigratesOnce |
| 25.93s | parallel | provenance | TestMatrix_FailureCheckpointRejectsCommittedJournal |
| 25.51s | parallel | provenance | TestConcurrentCreate |
| 24.95s | parallel | provenance | TestDBOSRegisteredBorrowedJournalRetryAndTerminalSemantics |
| 24.13s | parallel | provenance | TestDBOSCompletedRetryEveryCanonicalOperandHasZeroCallbackAndWrites |
| 20.24s | - | provenance/internal/sqlite | TestStartActivity_YAMLPermutations |
| 20.05s | parallel | provenance | TestRevokeVsInFlightCitationNoTOCTOU |
| 19.54s | parallel | provenance | TestNewDBOSAdapter_VersionMismatch |
| 18.57s | parallel | provenance | TestRegisterFixedSoftwareAgentConcurrentStartup |
| 18.10s | parallel | provenance | TestJournalFactsPublicFileBackedDecisionEvidenceSurviveReopen |
| 17.86s | parallel | provenance | TestCanonicalTaskStateSurvivesRestartAndReplay |
| 17.37s | parallel | provenance | TestPinnedSessionCreateAcrossIndependentHandles |
| 17.23s | parallel | provenance | TestOpenBorrowedSQLite_PreservesCallerPoolLimits |
| 17.19s | parallel | provenance | TestCanonicalRetryAcrossIndependentHandles |
| 17.09s | parallel | provenance | TestDemo_MultiProviderAgentsFromBestiary |
| 16.89s | parallel | provenance | TestCanonicalExactRetryAfterReopenReturnsCompleteResult |
| 16.31s | parallel | provenance | TestActivityCreate_ReopenReconstructsActivitySlot |
| 15.85s | parallel | provenance | TestRegisterFixedSoftwareAgentValidationCorpus |
| 14.82s | parallel | provenance | TestDeleteModeActivationSchemaFailureDoesNotPersistWAL |
| 14.64s | parallel | provenance | TestDeleteModeCorruptionPreflightIsByteAndModeReadOnly |
| 14.02s | parallel | provenance | TestCrashGap0_BeforeDomainCommit |
| 13.88s | serial | provenance | TestMatrix_SuccessCheckpointRejectsResultSlotDivergence |
| 13.59s | parallel | provenance | TestCrashGap1_DomainCommitBeforeCheckpoint |
| 13.52s | parallel | provenance | TestDBOSRetryYAMLValuesAreAuthoritative |
| 13.32s | parallel | provenance | TestCrashGap2_StepCheckpointBeforeCompletion |
| 12.43s | parallel | provenance | TestStandalone_OpenSQLiteMemory_SourceCompatible |
| 12.08s | parallel | provenance | TestDBOSSimultaneousExactAndChangedMutationRaces |
| 12.06s | - | provenance/internal/sqlite | TestFactQueriesCorruptionMatrixFailsClosedForBothSubtypes |
| 11.59s | parallel | provenance | TestRegisterFixedSoftwareAgentRollsBackEveryInsertBoundary |
| 11.13s | parallel | provenance | TestRegisterFixedSoftwareAgentErrorsAreActionable |
| 10.57s | parallel | provenance | TestDBOSBorrowedSQLitePool16SupportsConcurrentOperations |
| 10.25s | parallel | provenance | TestMatrix_AllocatedCreateChangedProvisionalUUIDReturnsOriginal |
| 10.02s | parallel | provenance | TestBorrowed_MigrationsCoexist_FreshExistingRepeat |
| 10.00s | parallel | provenance | TestMatrix_PresentSuccessConflict_Divergence |
| 9.87s | parallel | provenance | TestDBOSSmoke_ApplyCreatesTask |
| 9.78s | parallel | provenance | TestMatrix_AbsentExact_ReplaySucceeds |
| 9.75s | parallel | provenance | TestMatrix_PresentSuccessExact_ZeroCallback |
| 9.74s | parallel | provenance | TestMatrix_PresentSuccessMismatch_Divergence |
| 9.72s | serial | provenance | TestGovernedAllocationAcceptsDeepAcyclicAncestry |
| 9.57s | parallel | provenance | TestMatrix_AbsentAbsent_Succeeds |
| 9.53s | parallel | provenance | TestCancel_DeadlineWhileGated |
| 9.52s | parallel | provenance | TestActivityCreate_CollisionAttributionLookupErrorPropagates |
| 9.43s | parallel | provenance | TestBorrowed_ReadOnlyQueriesCreateNoEvent |
| 9.38s | parallel | provenance | TestDBOSExplicitResponseLossRetrievesCompleteResult |
| 9.38s | parallel | provenance | TestMatrix_AbsentConflict_TypedConflict |
| 9.37s | parallel | provenance | TestMatrix_TimestampOnlyRetryAttachesCompletedWorkflow |
| 9.30s | parallel | provenance | TestMatrix_PresentSuccessAbsent_Divergence |
| 9.20s | parallel | provenance | TestMatrix_PresentFailureOutcome_TypedFailurePermanent |
| 9.19s | parallel | provenance | TestMatrix_PresentSuccessChangedCanonicalOperand_ConflictsBeforeWorkflow |
| 9.17s | parallel | provenance | TestMatrix_UnknownLookupVariant_FailClosed |
| 9.01s | parallel | provenance | TestCanonicalRetryIgnoresCallerMutationDigestButRejectsEffectChange |
| 8.81s | parallel | provenance | TestAllocatedCreateReconcilesOnlyProvisionalUUID |
| 8.68s | serial | provenance | TestRepositoryTreeUsesEnduringVocabulary |
| 8.48s | serial | provenance | TestFusedGovernedAllocationRejectsForgedDBOSOutputAfterReopen |
| 7.87s | parallel | provenance | TestDemo_FullEpochSimulation |
| 7.59s | parallel | provenance | TestCorruptLegacySchemaStartupRollsBackWithoutByteDrift |
| 7.43s | parallel | provenance | TestInvalidDecisionEvidenceKindsFailBeforeJournalWrites |
| 7.34s | parallel | provenance | TestPinnedSessionCreatePreservesOperationIDContract |
| 7.32s | parallel | provenance | TestSession_AtomicStartEpisodeSetsOwner |
| 7.08s | parallel | provenance | TestSession_RelationshipVerbsAreJournaled |
| 7.06s | parallel | provenance | TestActivityCreate_BirthMappingFailureRollsBackActivity |
| 6.94s | parallel | provenance | TestStartupCanonicalValidationFailsClosedWithoutByteDrift |
| 6.89s | parallel | provenance | TestSession_ForcedTransitionUnderNonGoverningAuthorityRejected |
| 6.77s | parallel | provenance | TestFixedTaskCreateDoesNotReconcileCallerSuppliedID |
| 6.74s | parallel | provenance | TestActivityCreate_ExactReplay |
| 6.65s | parallel | provenance | TestSession_CloseTaskJournalsClosure |
| 6.62s | parallel | provenance | TestActivityCreate_ForeignOperationCollisionRollsBack |
| 6.61s | parallel | provenance | TestSession_CloseThenReopenConverges |
| 6.57s | parallel | provenance | TestSession_UpdateMetadataMaterializesAndJournals |
| 6.53s | parallel | provenance | TestDemo_PROVOActivities |
| 6.52s | parallel | provenance | TestSession_StartJournalsStarted |
| 6.52s | parallel | provenance | TestSession_StopHaltsInProgress |
| 6.50s | parallel | provenance | TestDemo_LabelsAndComments |
| 6.46s | parallel | provenance | TestSession_UpdateOwnerRejected |
| 6.45s | parallel | provenance | TestSession_UpdateEmptyIsNoOp |
| 6.44s | parallel | provenance | TestSession_PinnedOperationIDIsIdempotent |
| 6.43s | - | provenance/internal/sqlite | TestListTasks_YAMLPermutations |
| 6.42s | parallel | provenance | TestSession_IllegalTransitionRejected |
| 6.41s | parallel | provenance | TestSession_CreateJournalsBirth |
| 6.32s | parallel | provenance | TestJournalFactsPublicBorrowedFileBackedLiveness |
| 6.23s | parallel | provenance | TestCanonicalSQLConstraintsAreVersionAgnostic |
| 6.06s | parallel | provenance | TestSession_AtomicEmptyRejected |
| 5.97s | parallel | provenance | TestRegisterMLAgent |
| 5.96s | parallel | provenance | TestSession_CreateEmptyNamespaceRejected |
| 5.94s | parallel | provenance | TestDefaultRegistry_LookupRejectsBeforeDB |
| 5.90s | parallel | provenance | TestOpenBorrowedSQLite_SharesFileWithCaller |
| 5.82s | serial | provenance | TestGovernedAllocationV1ReducerParity |
| 5.82s | parallel | provenance | TestAgent |
| 5.81s | parallel | provenance | TestRevokeAndCitationBothOrderingsDeterministic |
| 5.78s | parallel | provenance | TestOpenMemory |
| 5.76s | parallel | provenance | TestWithModelRegistry_NilRegistry |
| 5.75s | parallel | provenance | TestSession_JournaledVerbsRequireGenesis |
| 5.66s | parallel | provenance | TestDemo_PROVOAgents |
| 5.54s | parallel | provenance | TestBorrowed_PostShutdown_SessionGated |
| 5.53s | parallel | provenance | TestBorrowed_PostShutdown_StoreUnavailable |
| 5.44s | serial | provenance | TestGovernedAllocationCommittedSuccessCannotBeReplacedByValidFailureArm |
| 5.09s | serial | provenance | TestOrdinaryJournalAuthorityBeyondGovernedDepthAndReplay |
| 4.65s | parallel | provenance | TestMigrationBaselineWaitsForConcurrentFileWriter |
| 4.41s | parallel | provenance | TestConcurrentSessionVsMigrationRaceFileBacked |
| 4.36s | parallel | provenance | TestConcurrentSessionVsMigrationRace |
| 4.29s | serial | provenance | TestDBOSAdapterTransferAssignmentRevocationRaceParity |
| 4.12s | - | provenance/internal/sqlite | TestFactQueryCorpusCoversBothSubtypesScopesDimensionsAndContexts |
| 4.04s | serial | provenance | TestDBOSRecoveredConditionAndActivityParity |
| 4.04s | parallel | provenance | TestEffectTaskCreate_ShadowReplayConverges |
| 3.83s | serial | provenance | TestDBOSAdapterTransferAssignmentSuccessAndReplayAcrossWorkflows |
| 3.80s | - | provenance/internal/sqlite | TestFactQueryPaginationTraversesJournalIDSnapshotExactlyOnce |
| 3.73s | - | provenance/internal/sqlite | TestFactQueryEvidenceSnapshotBarrierPinsPreCommitRows |
| 3.73s | - | provenance/internal/sqlite | TestFactQuerySurvivesCloseAndReopen |
| 3.71s | - | provenance/internal/sqlite | TestRegisterMLAgent_YAMLPermutations |
| 3.69s | - | provenance/internal/sqlite | TestFactQueryDecisionSnapshotBarrierPinsPreCommitRows |
| 3.66s | - | provenance/internal/sqlite | TestApplyWithoutDeadlineIsBoundedByBusyTimeoutNotTheCaller |
| 3.63s | serial | provenance | TestComposedReceiptDetectsForeignProducerMutationOnCanonicalAndExtraRows |
| 3.62s | - | provenance/internal/sqlite | TestFactQueryCorruptionFailsClosedWithoutChangingSQLiteArtifacts |
| 3.62s | serial | provenance | TestFusedGovernedAllocationComposedReducerAndParticipantFailuresRollBack |
| 3.49s | parallel | provenance | TestFoldDecisionEnforcesAuthorityGovernance |
| 3.43s | - | provenance/internal/sqlite | TestFactQueryReturnsCanonicalContextsAndUsesRequiredSubset |
| 3.41s | - | provenance/internal/sqlite | TestFactConditionsRequireSubtypeOwnedContexts |
| 3.37s | - | provenance/internal/sqlite | TestFactQueryPaginationPinsSnapshotAndCursor |
| 3.27s | parallel | provenance | TestFoldEvidenceEnforcesAuthorityGovernance |
| 3.26s | - | provenance/internal/sqlite | TestFactQueryScopesAndDimensionsUseOrWithinAndAcross |
| 3.23s | serial | provenance | TestGovernedClosureReopensAndCorruptionFailsClosed |
| 3.18s | parallel | provenance | TestResultSlotMatrix_TaskEvent |
| 3.17s | parallel | provenance | TestEffectTaskCreate_RejectsNonGoverningAuthority |
| 3.14s | parallel | provenance | TestEffectTaskCreate_JournalsBirth |
| 3.13s | - | provenance/internal/sqlite | TestFoldUpdate_YAMLPermutations |
| 3.08s | serial | provenance | TestFusedGovernedAllocationComposedForcedTransitionsSurviveReopen |
| 3.06s | parallel | provenance | TestResolveOperationIDInsertRaceTranslatesToTypedOutcome |
| 3.05s | serial | provenance | TestSessionTransferAssignmentConcurrentTransfersSingleWinner |
| 3.03s | parallel | provenance | TestApplyRejectsOperationIDReuseWithDifferentIdentity |
| 3.03s | parallel | provenance | TestResultSlotMatrix_DuplicateSlotRejected |
| 3.03s | parallel | provenance | TestDepTree |
| 3.00s | - | provenance/internal/sqlite | TestFactQueryReturnsExactDecisionAndEvidenceRows |
| 2.97s | - | provenance/internal/sqlite | TestSharedFactPageBindingUsesBoundedKindsSnapshotCursorAndContexts |
| 2.96s | serial | provenance | TestFusedGovernedAllocationComposedExactReopenReplayIsStableAndSkipsParticipant |
| 2.96s | parallel | provenance | TestDemo_DependencyGraph |
| 2.95s | parallel | provenance | TestResultSlotMatrix_MultipleSlots |
| 2.94s | serial | provenance | TestHostBoundGovernedAllocatorBorrowsEngineLifecycle |
| 2.94s | parallel | provenance | TestApplyConflictProducesTypedClosedSumAndErrorsAs |
| 2.91s | parallel | provenance | TestDemo_CycleDetection |
| 2.91s | parallel | provenance | TestActivityCreate_JournalsBirth |
| 2.91s | parallel | provenance | TestResultSlotMatrix_MissingSlotRejected |
| 2.85s | serial | provenance | TestGovernedAllocationStandaloneAndExactHandleFusedParity |
| 2.84s | serial | provenance | TestComposedActivityChronologyRejectsTamperingAcrossReplayAndReopen |
| 2.81s | - | provenance/internal/sqlite | TestFactConditionsObserveSuppliedTransactionAndRollback |
| 2.81s | - | provenance/internal/sqlite | TestConditionEvidenceSelector |
| 2.80s | - | provenance/internal/sqlite | TestExactFactConditionSuccess |
| 2.80s | parallel | provenance | TestResultSlotMatrix_Activity |
| 2.79s | - | provenance/internal/sqlite | TestFactQueryRejectsForgedSnapshotWatermark |
| 2.78s | serial | provenance | TestFusedGovernedAllocationComposedBatchReopenReplayPreservesOrder |
| 2.78s | - | provenance/internal/sqlite | TestFactQueryContextFilterStaysScopedToFactRows |
| 2.77s | serial | provenance | TestBoundGovernedAllocatorReopenReplaySuppressesParticipant |
| 2.77s | - | provenance/internal/sqlite | TestFactMatcherOnLeasedTransaction |
| 2.77s | parallel | provenance | TestEffectTaskCreate_RejectsDuplicateAndInvalid |
| 2.76s | - | provenance/internal/sqlite | TestConditionNonzeroFirstFailureIndex |
| 2.76s | - | provenance/internal/sqlite | TestCurrentFactConditionStale |
| 2.73s | - | provenance/internal/sqlite | TestConditionTaskScopeUnscoped |
| 2.72s | serial | provenance | TestFusedLegacyRunAllocateReopensBaselineWorkflowWithoutParticipant |
| 2.72s | - | provenance/internal/sqlite | TestExactFactConditionMissing |
| 2.71s | - | provenance/internal/sqlite | TestConditionExactTaskScopeFilter |
| 2.70s | parallel | provenance | TestDemo_ProvenanceEdges |
| 2.66s | - | provenance/internal/sqlite | TestCurrentFactConditionAbsenceSuccess |
| 2.65s | parallel | provenance | TestReadyAndBlocked |
| 2.64s | serial | provenance | TestSessionTransferAssignmentVsRevocationSingleWinner |
| 2.63s | - | provenance/internal/sqlite | TestExactFactConditionMismatch |
| 2.62s | parallel | provenance | TestAncestorsAndDescendants |
| 2.60s | - | provenance/internal/sqlite | TestConditionRollbackOnFailure |
| 2.58s | parallel | provenance | TestRemoveLabel |
| 2.56s | parallel | provenance | TestListFilterByLabel |
| 2.56s | parallel | provenance | TestList |
| 2.55s | parallel | provenance | TestRemoveEdge |
| 2.54s | parallel | provenance | TestListFilterByNamespace |
| 2.53s | parallel | provenance | TestDemo_CoreWorkflow |
| 2.51s | parallel | provenance | TestAddEdgeCycleDetected |
| 2.49s | parallel | provenance | TestAddLabel |
| 2.47s | parallel | provenance | TestNonBlockedByEdges |
| 2.44s | serial | provenance | TestGovernedAllocationBatchBoundariesRetriesAndOrder |
| 2.44s | parallel | provenance | TestRegisterFixedSoftwareAgentRejectsPreClaimActor |
| 2.43s | - | provenance/internal/sqlite | TestBorrowedReleaseRetiresConnectionWhenRestoreCannotLand |
| 2.37s | - | provenance/internal/sqlite | TestBorrowedLeaseEnforcesForeignKeysWhileHeld |
| 2.34s | parallel | provenance | TestCloseTaskAlreadyClosed |
| 2.26s | parallel | provenance | TestCreateGeneratesUUIDv7 |
| 2.26s | parallel | provenance | TestUpdateTask |
| 2.25s | parallel | provenance | TestCloseTask |
| 2.25s | parallel | provenance | TestAddComment |
| 2.22s | parallel | provenance | TestCreateAndShow |
| 2.21s | parallel | provenance | TestAddEdgeBlockedBy |
| 2.19s | parallel | provenance | TestStartAndEndActivity |
| 2.15s | parallel | provenance | TestCreateEmptyNamespace |
| 2.12s | parallel | provenance | TestShowNotFound |
| 2.06s | parallel | provenance | TestActivities |
| 2.06s | parallel | provenance | TestWithModelRegistry_CustomRegistry |
| 2.05s | parallel | provenance | TestRegisterSoftwareAgent |
| 2.04s | parallel | provenance | TestRegisterHumanAgent |
| 2.04s | parallel | provenance | TestRegisterFixedSoftwareAgentManifestConflictIsActionableOnce |
| 2.02s | serial | provenance | TestBoundGovernedAllocatorUsesHostRootAndReportsReplay |
| 2.01s | serial | provenance | TestFusedComposedBatchConditionIsCanonicalAtomicAndReplaySafe |
| 1.99s | serial | provenance | TestComposedGovernedAllocationReplayReceiptAndCopies |
| 1.94s | parallel | provenance | TestRegisterSoftwareAgentRandomIDPathUnchanged |
| 1.93s | serial | provenance | TestGovernedAllocationLateActivityFailureRollsBackWholeV1Fold |
| 1.91s | serial | provenance | TestFusedGovernedAllocationComposedBatchCommitsOrderedCompleteClosureAndReplays |
| 1.91s | parallel | provenance | TestRegisterFixedSoftwareAgentOverlapIsActionableOnce |
| 1.91s | parallel | provenance | TestRegisterFixedSoftwareAgentReplayRepairAndDrift |
| 1.90s | serial | provenance | TestDBOSAdapterTransferAssignmentChangedInputConflicts |
| 1.88s | serial | provenance | TestBorrowedTrackerCloseInvalidatesOnlyLocalTracker |
| 1.88s | serial | provenance | TestFusedGovernedAllocationComposedConflictsAndDefensiveCopies |
| 1.87s | serial | provenance | TestProductionSQLTextIsDecidableFromSource |
| 1.85s | serial | provenance | TestGovernedAllocationV1CloseGateMatchesOrdinaryReducer |
| 1.85s | serial | provenance | TestGenericReservedIdentityPreservesOnlyUnmarkedHistoricalReplay |
| 1.84s | - | provenance/internal/sqlite | TestFactContextLegacyActivationBackfillsCanonicalOnly |
| 1.84s | parallel | provenance | TestWithModelRegistry_EmptyRegistry |
| 1.83s | serial | provenance | TestFusedGovernedAllocationParticipantErrorRollsBackDomainAuditAndSuccessfulCheckpoint |
| 1.83s | serial | provenance | TestComposedConflictProofRejectsMutatedAuthorityOwnerAndSupplement |
| 1.83s | serial | provenance | TestDBOSAdapterReservedIdentityAdmission |
| 1.83s | serial | provenance | TestFusedGovernedAllocationParticipantExactReplaySkipsCallbackAndDistinctWorkflowIsIdempotent |
| 1.82s | - | provenance/internal/sqlite | TestFactQueryRejectsInvalidBoundsBeforeOpeningAConnection |
| 1.82s | serial | provenance | TestFusedGovernedAllocationComposedPersistsAllowedSupplementsAndReplays |
| 1.80s | serial | provenance | TestComposedGovernedAllocationExactReceiptMissingOwnerMarkerIsCorruption |
| 1.79s | serial | provenance | TestDBOSActivityConflictIsCheckpointedTypedAndActivityResultTransports |
| 1.79s | serial | provenance | TestGovernedAllocationReceiptRejectsCanonicalTaskProjectionTampering |
| 1.79s | serial | provenance | TestFusedComposedReferenceScopeProvesDescendantAndRejectsUnrelated |
| 1.79s | serial | provenance | TestComposedParticipantFailureRollsBackEveryGovernedTable |
| 1.78s | serial | provenance | TestJoinedParticipantAndCleanupFailureCannotAuthenticateDomainRejection |
| 1.78s | serial | provenance | TestComposedAllocationReservesItsDerivedInternalOperationID |
| 1.78s | serial | provenance | TestFusedGovernedAllocationParticipantReceivesDefensiveRequestAndClosureCopies |
| 1.76s | serial | provenance | TestComposedGovernedAllocationOperationShapeConflictsBeforeOwnerMarker |
| 1.76s | - | provenance/internal/sqlite | TestFactContextRowStartupFailuresPreserveFiles |
| 1.75s | serial | provenance | TestGovernedAllocationComposedRejectsMoreThanOneChild |
| 1.75s | - | provenance/internal/sqlite | TestFactContextIntegrityRejectsStoredCorruption |
| 1.75s | serial | provenance | TestFusedGovernedAllocationParticipantCommitsDomainAuditAndCheckpoint |
| 1.74s | serial | provenance | TestCancel_WhileGated_DurableWorkContinues |
| 1.74s | serial | provenance | TestFreshWorkflowRejectsWrongParentAuthorityBeforeWrites |
| 1.74s | serial | provenance | TestFusedWorkflowIDReplayMatchesCanonicalRequestAndAuthority |
| 1.73s | - | provenance/internal/journal | TestMutationV1EveryResourceBoundaryIsExact |
| 1.73s | serial | provenance | TestGovernedAllocationComposedRejectsUnsupportedAndUnrelatedReferencesBeforeAllocation |
| 1.71s | serial | provenance | TestDBOSApplyRejectsDuplicateStoredInputBeforeCallbacksOrWrites |
| 1.69s | - | provenance/internal/sqlite | TestFactQueriesRejectMalformedInputBeforeConnectionLease |
| 1.69s | serial | provenance | TestDBOSConditionFailureIsCheckpointedTypedAndPermanent |
| 1.68s | serial | provenance | TestSessionGovernedIngressRejectsReservedOperationIDsWithoutWrites |
| 1.67s | - | provenance/internal/helpers | TestAncestors_GraphTopologies |
| 1.66s | serial | provenance | TestDBOSDurableSnapshotDetectsTaskAttributionMutation |
| 1.66s | serial | provenance | TestComposedGovernedAllocationRejectsEmptyBeforeDBOS |
| 1.64s | serial | provenance | TestRunInitializeRootRejectsReservedOperationIDBeforeDBOS |
| 1.64s | serial | provenance | TestGovernedPublicIngressRejectsReservedOperationIDsBeforeDBOS |
| 1.61s | serial | provenance | TestCancel_AlreadyCancelled_StartsNothing |
| 1.61s | serial | provenance | TestFusedGovernedAllocationComposedBatchInvalidSecondChildWritesNothing |
| 1.60s | - | provenance/internal/helpers | TestDescendants_GraphTopologies |
| 1.59s | - | provenance/internal/sqlite | TestSuppressCheckConstraintsRestoresAndReportsBoth |
| 1.55s | serial | provenance | TestNewDBOSAdapterRejectsInvalidConfigBeforeRegistration |
| 1.51s | - | provenance/internal/sqlite | TestPauseForeignKeysRestoresAndReportsBoth |
| 1.45s | - | provenance/internal/sqlite | TestCurrentFactConditionSuccess |
| 1.39s | - | provenance/internal/sqlite | TestFactMatcherLookupFailureRollsBackCleanly |
| 1.26s | serial | provenance | TestMigrationColumnAddPathForColumnlessLegacyDB |
| 1.23s | - | provenance/internal/sqlite | TestFactContextStartupFailuresPreserveFiles |
| 1.16s | serial | provenance | TestSessionAllocationReplayRequiresExactAuthorityAndSurvivesLaterRevocation |
| 1.14s | serial | provenance | TestGovernedAllocationRejectsCyclicAncestryWithoutWrites |
| 1.11s | serial | provenance | TestSessionAllocateGovernedComposedUsesSameReducer |
| 1.10s | serial | provenance | TestGovernedGenesisRetryAndConflictingSecondGenesis |
| 1.09s | - | provenance/internal/sqlite | TestOwnedPoolLeaseReArmsForeignKeys |
| 1.08s | serial | provenance | TestGovernedAllocationRejectsRevokedMiddleAncestorWithoutWrites |
| 1.07s | serial | provenance | TestGovernedAllocationRejectsRevocationWithoutWrites |
| 1.06s | serial | provenance | TestSessionAllocateGovernedRejectsDifferentActiveParentAuthorityWithoutWrites |
| 1.05s | serial | provenance | TestGovernedAllocationRejectsBeforeWriting |
| 1.04s | serial | provenance | TestExternalAtomicJournalContractCompiles |
| 1.02s | serial | provenance | TestSQLGuardRejectsEverySeededViolationClass |
