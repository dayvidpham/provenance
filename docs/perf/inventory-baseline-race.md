# Baseline timing inventory - race suite (commit 8bf1a9b)

## Per-package elapsed

| package | elapsed |
|---|---:|
| github.com/dayvidpham/provenance | 411.23s |
| github.com/dayvidpham/provenance/internal/sqlite | 95.66s |
| github.com/dayvidpham/provenance/internal/helpers | 6.44s |
| github.com/dayvidpham/provenance/internal/graph | 3.69s |
| github.com/dayvidpham/provenance/internal/fusedtx | 3.27s |
| github.com/dayvidpham/provenance/internal/journal | 2.86s |
| github.com/dayvidpham/provenance/internal/allocation | 1.43s |
| github.com/dayvidpham/provenance/pkg/namespace | 1.03s |
| github.com/dayvidpham/provenance/pkg/ptypes | 1.03s |
| github.com/dayvidpham/provenance/internal/testcorpus | 1.01s |

## Root package: serial vs parallel top-level tests

- serial tests: 133, sum elapsed **330.57s** (critical-path floor)
- parallel tests: 165, sum elapsed 1632.30s
- package wall: 411.23s

## Serial-phase cost by file

| file | tests | elapsed |
|---|---:|---:|
| governed_allocation_integration_test.go | 23 | 151.76s |
| governed_allocation_failure_arm_integrity_test.go | 5 | 21.92s |
| dbos_matrix_test.go | 1 | 18.05s |
| governed_allocation_composed_batch_test.go | 6 | 13.34s |
| governed_allocation_depth_test.go | 2 | 12.68s |
| dbos_assignment_transfer_test.go | 3 | 10.94s |
| governed_allocation_identity_replay_test.go | 5 | 9.48s |
| assignment_transfer_test.go | 7 | 9.12s |
| governed_allocation_bound_runtime_test.go | 3 | 8.78s |
| hygiene_test.go | 3 | 8.74s |
| governed_allocation_reducer_parity_test.go | 2 | 8.18s |
| governed_allocation_authority_boundary_test.go | 1 | 5.37s |
| dbos_recovery_parity_test.go | 1 | 4.52s |
| governed_allocation_receipt_integrity_test.go | 2 | 4.49s |
| dbos_cancel_test.go | 2 | 4.24s |
| dbos_apply_parity_test.go | 3 | 4.08s |
| governed_allocation_shape_conflict_test.go | 2 | 3.93s |
| governed_allocation_composed_contract_test.go | 3 | 3.72s |
| sql_architecture_test.go | 4 | 3.12s |
| governed_allocation_composed_failure_test.go | 1 | 3.01s |
| governed_allocation_forced_replay_test.go | 1 | 2.82s |
| governed_allocation_fused_legacy_replay_test.go | 1 | 2.64s |
| governed_allocation_atomic_rollback_test.go | 1 | 2.36s |
| dbos_durable_deep_test.go | 1 | 2.26s |
| dbos_harness_test.go | 1 | 2.09s |
| dbos_config_test.go | 3 | 1.91s |
| dbos_family_retry_test.go | 3 | 1.75s |
| journal_watermark_test.go | 1 | 1.51s |
| journal_contract_compile_test.go | 1 | 1.19s |
| tracker_test.go | 1 | 1.03s |
| create_permutation_test.go | 1 | 0.76s |
| dbos_exhaustive_retry_test.go | 1 | 0.38s |
| dbos_failure_contract_test.go | 5 | 0.12s |
| dbos_contract_fixture_test.go | 5 | 0.09s |
| dbos_compilefail_test.go | 1 | 0.08s |
| reexport_test.go | 3 | 0.04s |
| journal_corpus_test.go | 1 | 0.04s |
| dbos_internal_test.go | 10 | 0.02s |
| ast_grep_rules_test.go | 2 | 0.01s |
| error_message_oracle_test.go | 2 | 0.00s |
| journal_concurrent_writers_test.go | 4 | 0.00s |
| dbos_crashgap_test.go | 1 | 0.00s |
| adapter_test.go | 2 | 0.00s |
| canonical_allocation_proof_test.go | 1 | 0.00s |
| journal_authority_revocation_race_test.go | 1 | 0.00s |

## DBOS context launches (total 161)

| launches | test |
|---:|---|
| 51 | TestFusedGovernedAllocationComposedRejectsStructurallyForgedSQLiteReceipts |
| 10 | TestDBOSRetryYAMLValuesAreAuthoritative |
| 8 | TestMatrix_SuccessCheckpointRejectsResultSlotDivergence |
| 6 | TestFusedGovernedAllocationRejectsForgedDBOSOutputAfterReopen |
| 4 | TestGovernedAllocationCommittedSuccessCannotBeReplacedByValidFailureArm |
| 4 | TestDBOSCompletedRetryEveryCanonicalOperandHasZeroCallbackAndWrites |
| 3 | TestDBOSAdapterTransferAssignmentSuccessAndReplayAcrossWorkflows |
| 3 | TestDBOSCompletedRetryUsesOneValidBaselineAndOneFieldChangePerFamily |
| 2 | TestComposedReceiptDetectsForeignProducerMutationOnCanonicalAndExtraRows |
| 2 | TestFusedGovernedAllocationComposedExactReopenReplayIsStableAndSkipsParticipant |
| 2 | TestComposedActivityChronologyRejectsTamperingAcrossReplayAndReopen |
| 2 | TestFusedGovernedAllocationComposedForcedTransitionsSurviveReopen |
| 2 | TestDBOSRecoveredConditionAndActivityParity |
| 2 | TestFusedGovernedAllocationComposedBatchReopenReplayPreservesOrder |
| 2 | TestBoundGovernedAllocatorReopenReplaySuppressesParticipant |
| 2 | TestFusedLegacyRunAllocateReopensBaselineWorkflowWithoutParticipant |
| 2 | TestFusedGovernedAllocationComposedReducerAndParticipantFailuresRollBack |
| 2 | TestDBOSSimultaneousExactAndChangedMutationRaces |
| 2 | TestRegisterFixedSoftwareAgentValidationCorpus |
| 2 | TestContractCorpusExecutesImplementedPartitions |

## All top-level tests > 1s

| elapsed | mode | package | test |
|---:|---|---|---|
| 100.74s | serial | provenance | TestFusedGovernedAllocationComposedRejectsStructurallyForgedSQLiteReceipts |
| 55.99s | parallel | provenance | TestAllocatedCreateCorruptionFailsLiveAndOnOpenWithoutDrift |
| 53.50s | parallel | provenance | TestCanonicalRetryMatrixSameProcessAndReopen |
| 42.06s | parallel | provenance | TestMixedLegacyCanonicalMalformedPairsFailWithoutByteDrift |
| 41.23s | parallel | provenance | TestDBOSCompletedRetryUsesOneValidBaselineAndOneFieldChangePerFamily |
| 41.19s | parallel | provenance | TestStartupCorruptionMatrixLeavesBytesUnchanged |
| 41.04s | parallel | provenance | TestCanonicalSchemaMigrationAndMixedLegacyRowsAreIdempotent |
| 39.88s | parallel | provenance | TestCanonicalColumnPreflightErrorsAreTypedActionableAndReadOnly |
| 39.50s | parallel | provenance | TestMatrix_FailureCheckpointRejectsCommittedJournal |
| 39.25s | parallel | provenance | TestCanonicalRetryMatrixSimultaneousIndependentHandles |
| 35.68s | parallel | provenance | TestPinnedSessionCreateSameProcessAndReopenUUIDv7 |
| 31.03s | parallel | provenance | TestMissingJournalOperationFKMigrationIsComposableAndIdempotent |
| 30.79s | parallel | provenance | TestDemo_Persistence |
| 30.25s | parallel | provenance | TestAllocatedCreateApplyReturnsCompleteResultAcrossRetryModes |
| 29.11s | parallel | provenance | TestV1SpecificSQLAuthorityMigratesOnce |
| 26.77s | parallel | provenance | TestConcurrentCreate |
| 25.61s | parallel | provenance | TestDBOSRegisteredBorrowedJournalRetryAndTerminalSemantics |
| 25.03s | parallel | provenance | TestDBOSCompletedRetryEveryCanonicalOperandHasZeroCallbackAndWrites |
| 24.12s | - | provenance/internal/sqlite | TestStartActivity_YAMLPermutations |
| 21.58s | parallel | provenance | TestRevokeVsInFlightCitationNoTOCTOU |
| 20.69s | parallel | provenance | TestNewDBOSAdapter_VersionMismatch |
| 19.32s | parallel | provenance | TestOpenBorrowedSQLite_PreservesCallerPoolLimits |
| 19.26s | parallel | provenance | TestJournalFactsPublicFileBackedDecisionEvidenceSurviveReopen |
| 19.25s | parallel | provenance | TestRegisterFixedSoftwareAgentConcurrentStartup |
| 18.64s | parallel | provenance | TestDemo_MultiProviderAgentsFromBestiary |
| 18.26s | parallel | provenance | TestPinnedSessionCreateAcrossIndependentHandles |
| 18.23s | parallel | provenance | TestCanonicalTaskStateSurvivesRestartAndReplay |
| 18.05s | serial | provenance | TestMatrix_SuccessCheckpointRejectsResultSlotDivergence |
| 17.99s | parallel | provenance | TestCanonicalRetryAcrossIndependentHandles |
| 17.97s | parallel | provenance | TestActivityCreate_ReopenReconstructsActivitySlot |
| 17.62s | parallel | provenance | TestRegisterFixedSoftwareAgentValidationCorpus |
| 17.57s | parallel | provenance | TestCanonicalExactRetryAfterReopenReturnsCompleteResult |
| 16.74s | parallel | provenance | TestDeleteModeCorruptionPreflightIsByteAndModeReadOnly |
| 15.52s | parallel | provenance | TestDeleteModeActivationSchemaFailureDoesNotPersistWAL |
| 15.07s | - | provenance/internal/sqlite | TestFactQueriesCorruptionMatrixFailsClosedForBothSubtypes |
| 14.63s | parallel | provenance | TestCrashGap1_DomainCommitBeforeCheckpoint |
| 14.24s | parallel | provenance | TestCrashGap0_BeforeDomainCommit |
| 14.03s | parallel | provenance | TestDBOSRetryYAMLValuesAreAuthoritative |
| 13.98s | parallel | provenance | TestCrashGap2_StepCheckpointBeforeCompletion |
| 13.03s | parallel | provenance | TestRegisterFixedSoftwareAgentRollsBackEveryInsertBoundary |
| 12.81s | parallel | provenance | TestStandalone_OpenSQLiteMemory_SourceCompatible |
| 12.58s | parallel | provenance | TestDBOSSimultaneousExactAndChangedMutationRaces |
| 12.32s | parallel | provenance | TestRegisterFixedSoftwareAgentErrorsAreActionable |
| 11.46s | parallel | provenance | TestBorrowed_MigrationsCoexist_FreshExistingRepeat |
| 11.40s | serial | provenance | TestGovernedAllocationAcceptsDeepAcyclicAncestry |
| 11.36s | serial | provenance | TestGovernedAllocationCommittedSuccessCannotBeReplacedByValidFailureArm |
| 11.31s | parallel | provenance | TestDBOSBorrowedSQLitePool16SupportsConcurrentOperations |
| 10.86s | parallel | provenance | TestCancel_DeadlineWhileGated |
| 10.63s | parallel | provenance | TestMatrix_AllocatedCreateChangedProvisionalUUIDReturnsOriginal |
| 10.61s | parallel | provenance | TestMatrix_AbsentExact_ReplaySucceeds |
| 10.57s | parallel | provenance | TestDBOSSmoke_ApplyCreatesTask |
| 10.55s | parallel | provenance | TestMatrix_PresentSuccessChangedCanonicalOperand_ConflictsBeforeWorkflow |
| 10.40s | parallel | provenance | TestMatrix_TimestampOnlyRetryAttachesCompletedWorkflow |
| 10.36s | parallel | provenance | TestMatrix_PresentSuccessConflict_Divergence |
| 10.18s | parallel | provenance | TestMatrix_PresentSuccessAbsent_Divergence |
| 10.09s | parallel | provenance | TestDBOSExplicitResponseLossRetrievesCompleteResult |
| 10.08s | parallel | provenance | TestMatrix_PresentFailureOutcome_TypedFailurePermanent |
| 10.05s | parallel | provenance | TestMatrix_PresentSuccessExact_ZeroCallback |
| 10.01s | parallel | provenance | TestBorrowed_ReadOnlyQueriesCreateNoEvent |
| 9.99s | parallel | provenance | TestActivityCreate_CollisionAttributionLookupErrorPropagates |
| 9.95s | parallel | provenance | TestMatrix_AbsentConflict_TypedConflict |
| 9.91s | parallel | provenance | TestMatrix_AbsentAbsent_Succeeds |
| 9.89s | parallel | provenance | TestMatrix_PresentSuccessMismatch_Divergence |
| 9.85s | parallel | provenance | TestMatrix_UnknownLookupVariant_FailClosed |
| 9.42s | parallel | provenance | TestCanonicalRetryIgnoresCallerMutationDigestButRejectsEffectChange |
| 9.26s | parallel | provenance | TestAllocatedCreateReconcilesOnlyProvisionalUUID |
| 9.02s | serial | provenance | TestFusedGovernedAllocationRejectsForgedDBOSOutputAfterReopen |
| 8.74s | serial | provenance | TestRepositoryTreeUsesEnduringVocabulary |
| 8.54s | parallel | provenance | TestDemo_FullEpochSimulation |
| 8.16s | parallel | provenance | TestCorruptLegacySchemaStartupRollsBackWithoutByteDrift |
| 8.08s | parallel | provenance | TestActivityCreate_BirthMappingFailureRollsBackActivity |
| 7.79s | parallel | provenance | TestInvalidDecisionEvidenceKindsFailBeforeJournalWrites |
| 7.76s | parallel | provenance | TestStartupCanonicalValidationFailsClosedWithoutByteDrift |
| 7.66s | parallel | provenance | TestPinnedSessionCreatePreservesOperationIDContract |
| 7.50s | parallel | provenance | TestSession_CloseThenReopenConverges |
| 7.45s | parallel | provenance | TestSession_RelationshipVerbsAreJournaled |
| 7.42s | parallel | provenance | TestSession_ForcedTransitionUnderNonGoverningAuthorityRejected |
| 7.39s | parallel | provenance | TestSession_StopHaltsInProgress |
| 7.33s | parallel | provenance | TestFixedTaskCreateDoesNotReconcileCallerSuppliedID |
| 7.12s | parallel | provenance | TestSession_AtomicStartEpisodeSetsOwner |
| 7.10s | parallel | provenance | TestJournalFactsPublicBorrowedFileBackedLiveness |
| 7.04s | parallel | provenance | TestSession_PinnedOperationIDIsIdempotent |
| 6.95s | parallel | provenance | TestDemo_LabelsAndComments |
| 6.94s | parallel | provenance | TestSession_CloseTaskJournalsClosure |
| 6.93s | - | provenance/internal/sqlite | TestListTasks_YAMLPermutations |
| 6.93s | parallel | provenance | TestSession_StartJournalsStarted |
| 6.89s | parallel | provenance | TestSession_IllegalTransitionRejected |
| 6.87s | parallel | provenance | TestActivityCreate_ExactReplay |
| 6.85s | parallel | provenance | TestSession_UpdateOwnerRejected |
| 6.83s | parallel | provenance | TestDemo_PROVOAgents |
| 6.81s | parallel | provenance | TestSession_UpdateMetadataMaterializesAndJournals |
| 6.77s | parallel | provenance | TestActivityCreate_ForeignOperationCollisionRollsBack |
| 6.76s | parallel | provenance | TestDemo_PROVOActivities |
| 6.67s | parallel | provenance | TestSession_CreateJournalsBirth |
| 6.62s | parallel | provenance | TestCanonicalSQLConstraintsAreVersionAgnostic |
| 6.61s | parallel | provenance | TestSession_CreateEmptyNamespaceRejected |
| 6.58s | parallel | provenance | TestSession_AtomicEmptyRejected |
| 6.41s | parallel | provenance | TestSession_UpdateEmptyIsNoOp |
| 6.39s | parallel | provenance | TestAgent |
| 6.28s | parallel | provenance | TestBorrowed_PostShutdown_SessionGated |
| 6.26s | parallel | provenance | TestDefaultRegistry_LookupRejectsBeforeDB |
| 6.24s | parallel | provenance | TestSession_JournaledVerbsRequireGenesis |
| 6.24s | parallel | provenance | TestOpenBorrowedSQLite_SharesFileWithCaller |
| 6.23s | parallel | provenance | TestWithModelRegistry_NilRegistry |
| 6.22s | parallel | provenance | TestBorrowed_PostShutdown_StoreUnavailable |
| 6.16s | parallel | provenance | TestRegisterMLAgent |
| 6.09s | parallel | provenance | TestOpenMemory |
| 5.94s | parallel | provenance | TestRevokeAndCitationBothOrderingsDeterministic |
| 5.82s | serial | provenance | TestGovernedAllocationV1ReducerParity |
| 5.37s | serial | provenance | TestOrdinaryJournalAuthorityBeyondGovernedDepthAndReplay |
| 5.15s | - | provenance/internal/sqlite | TestFactQueryCorpusCoversBothSubtypesScopesDimensionsAndContexts |
| 5.09s | serial | provenance | TestComposedReceiptDetectsForeignProducerMutationOnCanonicalAndExtraRows |
| 5.05s | parallel | provenance | TestMigrationBaselineWaitsForConcurrentFileWriter |
| 5.03s | serial | provenance | TestDBOSAdapterTransferAssignmentRevocationRaceParity |
| 4.87s | parallel | provenance | TestConcurrentSessionVsMigrationRaceFileBacked |
| 4.79s | parallel | provenance | TestConcurrentSessionVsMigrationRace |
| 4.74s | - | provenance/internal/sqlite | TestFactQueryDecisionSnapshotBarrierPinsPreCommitRows |
| 4.73s | - | provenance/internal/sqlite | TestFactQueryPaginationTraversesJournalIDSnapshotExactlyOnce |
| 4.62s | - | provenance/internal/sqlite | TestFactQueryCorruptionFailsClosedWithoutChangingSQLiteArtifacts |
| 4.62s | - | provenance/internal/sqlite | TestApplyWithoutDeadlineIsBoundedByBusyTimeoutNotTheCaller |
| 4.52s | serial | provenance | TestDBOSRecoveredConditionAndActivityParity |
| 4.48s | - | provenance/internal/sqlite | TestFactQueryReturnsCanonicalContextsAndUsesRequiredSubset |
| 4.32s | - | provenance/internal/sqlite | TestFactConditionsRequireSubtypeOwnedContexts |
| 4.31s | - | provenance/internal/sqlite | TestFactQueryPaginationPinsSnapshotAndCursor |
| 4.16s | parallel | provenance | TestEffectTaskCreate_ShadowReplayConverges |
| 4.13s | - | provenance/internal/sqlite | TestFactQueryScopesAndDimensionsUseOrWithinAndAcross |
| 4.08s | serial | provenance | TestDBOSAdapterTransferAssignmentSuccessAndReplayAcrossWorkflows |
| 3.98s | - | provenance/internal/sqlite | TestFactQueryReturnsExactDecisionAndEvidenceRows |
| 3.84s | - | provenance/internal/sqlite | TestRegisterMLAgent_YAMLPermutations |
| 3.78s | - | provenance/internal/sqlite | TestSharedFactPageBindingUsesBoundedKindsSnapshotCursorAndContexts |
| 3.75s | - | provenance/internal/sqlite | TestFactMatcherLookupFailureRollsBackCleanly |
| 3.70s | parallel | provenance | TestFoldDecisionEnforcesAuthorityGovernance |
| 3.68s | serial | provenance | TestFusedGovernedAllocationComposedReducerAndParticipantFailuresRollBack |
| 3.66s | - | provenance/internal/sqlite | TestCurrentFactConditionStale |
| 3.62s | - | provenance/internal/sqlite | TestExactFactConditionMismatch |
| 3.61s | - | provenance/internal/sqlite | TestExactFactConditionSuccess |
| 3.61s | - | provenance/internal/sqlite | TestConditionExactTaskScopeFilter |
| 3.60s | - | provenance/internal/sqlite | TestFactMatcherOnLeasedTransaction |
| 3.59s | - | provenance/internal/sqlite | TestConditionTaskScopeUnscoped |
| 3.56s | - | provenance/internal/sqlite | TestConditionEvidenceSelector |
| 3.54s | - | provenance/internal/sqlite | TestCurrentFactConditionAbsenceSuccess |
| 3.54s | - | provenance/internal/sqlite | TestCurrentFactConditionSuccess |
| 3.53s | - | provenance/internal/sqlite | TestExactFactConditionMissing |
| 3.48s | - | provenance/internal/sqlite | TestFactConditionsObserveSuppliedTransactionAndRollback |
| 3.46s | - | provenance/internal/sqlite | TestConditionRollbackOnFailure |
| 3.43s | - | provenance/internal/sqlite | TestConditionNonzeroFirstFailureIndex |
| 3.42s | parallel | provenance | TestFoldEvidenceEnforcesAuthorityGovernance |
| 3.38s | - | provenance/internal/sqlite | TestFoldUpdate_YAMLPermutations |
| 3.38s | serial | provenance | TestGovernedClosureReopensAndCorruptionFailsClosed |
| 3.36s | parallel | provenance | TestResultSlotMatrix_MultipleSlots |
| 3.32s | serial | provenance | TestSessionTransferAssignmentConcurrentTransfersSingleWinner |
| 3.30s | parallel | provenance | TestResolveOperationIDInsertRaceTranslatesToTypedOutcome |
| 3.26s | parallel | provenance | TestApplyConflictProducesTypedClosedSumAndErrorsAs |
| 3.26s | parallel | provenance | TestDemo_DependencyGraph |
| 3.25s | parallel | provenance | TestEffectTaskCreate_JournalsBirth |
| 3.24s | serial | provenance | TestFusedGovernedAllocationComposedExactReopenReplayIsStableAndSkipsParticipant |
| 3.24s | parallel | provenance | TestResultSlotMatrix_DuplicateSlotRejected |
| 3.22s | parallel | provenance | TestActivityCreate_JournalsBirth |
| 3.20s | parallel | provenance | TestEffectTaskCreate_RejectsDuplicateAndInvalid |
| 3.16s | serial | provenance | TestHostBoundGovernedAllocatorBorrowsEngineLifecycle |
| 3.15s | parallel | provenance | TestApplyRejectsOperationIDReuseWithDifferentIdentity |
| 3.15s | parallel | provenance | TestResultSlotMatrix_Activity |
| 3.15s | parallel | provenance | TestRemoveLabel |
| 3.13s | parallel | provenance | TestResultSlotMatrix_TaskEvent |
| 3.07s | parallel | provenance | TestResultSlotMatrix_MissingSlotRejected |
| 3.07s | parallel | provenance | TestEffectTaskCreate_RejectsNonGoverningAuthority |
| 3.01s | serial | provenance | TestComposedActivityChronologyRejectsTamperingAcrossReplayAndReopen |
| 3.01s | serial | provenance | TestGovernedAllocationStandaloneAndExactHandleFusedParity |
| 2.98s | parallel | provenance | TestDepTree |
| 2.95s | serial | provenance | TestSessionTransferAssignmentVsRevocationSingleWinner |
| 2.92s | serial | provenance | TestBoundGovernedAllocatorReopenReplaySuppressesParticipant |
| 2.91s | - | provenance/internal/sqlite | TestBorrowedLeaseEnforcesForeignKeysWhileHeld |
| 2.89s | parallel | provenance | TestDemo_ProvenanceEdges |
| 2.86s | parallel | provenance | TestDemo_CycleDetection |
| 2.85s | serial | provenance | TestFusedGovernedAllocationComposedBatchReopenReplayPreservesOrder |
| 2.82s | serial | provenance | TestFusedGovernedAllocationComposedForcedTransitionsSurviveReopen |
| 2.82s | parallel | provenance | TestRegisterFixedSoftwareAgentRejectsPreClaimActor |
| 2.77s | parallel | provenance | TestAncestorsAndDescendants |
| 2.76s | parallel | provenance | TestReadyAndBlocked |
| 2.75s | parallel | provenance | TestRemoveEdge |
| 2.72s | parallel | provenance | TestListFilterByNamespace |
| 2.70s | serial | provenance | TestBoundGovernedAllocatorUsesHostRootAndReportsReplay |
| 2.68s | parallel | provenance | TestList |
| 2.65s | - | provenance/internal/sqlite | TestFactQuerySurvivesCloseAndReopen |
| 2.64s | serial | provenance | TestGovernedAllocationReceiptRejectsCanonicalTaskProjectionTampering |
| 2.64s | serial | provenance | TestFusedLegacyRunAllocateReopensBaselineWorkflowWithoutParticipant |
| 2.64s | parallel | provenance | TestAddEdgeBlockedBy |
| 2.63s | - | provenance/internal/sqlite | TestFactQueryEvidenceSnapshotBarrierPinsPreCommitRows |
| 2.60s | parallel | provenance | TestAddComment |
| 2.59s | parallel | provenance | TestListFilterByLabel |
| 2.58s | parallel | provenance | TestAddEdgeCycleDetected |
| 2.57s | parallel | provenance | TestNonBlockedByEdges |
| 2.54s | parallel | provenance | TestCreateGeneratesUUIDv7 |
| 2.49s | parallel | provenance | TestCloseTaskAlreadyClosed |
| 2.49s | parallel | provenance | TestAddLabel |
| 2.48s | - | provenance/internal/sqlite | TestFactContextLegacyActivationBackfillsCanonicalOnly |
| 2.48s | serial | provenance | TestGovernedAllocationBatchBoundariesRetriesAndOrder |
| 2.47s | serial | provenance | TestFusedGovernedAllocationParticipantCommitsDomainAuditAndCheckpoint |
| 2.47s | parallel | provenance | TestStartAndEndActivity |
| 2.46s | - | provenance/internal/sqlite | TestOwnedPoolLeaseReArmsForeignKeys |
| 2.45s | - | provenance/internal/sqlite | TestFactQueryRejectsInvalidBoundsBeforeOpeningAConnection |
| 2.44s | - | provenance/internal/sqlite | TestPauseForeignKeysRestoresAndReportsBoth |
| 2.43s | - | provenance/internal/sqlite | TestFactQueriesRejectMalformedInputBeforeConnectionLease |
| 2.43s | parallel | provenance | TestDemo_CoreWorkflow |
| 2.41s | parallel | provenance | TestUpdateTask |
| 2.39s | parallel | provenance | TestCreateAndShow |
| 2.37s | serial | provenance | TestComposedAllocationReservesItsDerivedInternalOperationID |
| 2.37s | - | provenance/internal/sqlite | TestFactContextRowStartupFailuresPreserveFiles |
| 2.36s | serial | provenance | TestGenericReservedIdentityPreservesOnlyUnmarkedHistoricalReplay |
| 2.36s | serial | provenance | TestGovernedAllocationComposedRejectsMoreThanOneChild |
| 2.36s | serial | provenance | TestGovernedAllocationV1CloseGateMatchesOrdinaryReducer |
| 2.36s | serial | provenance | TestGovernedAllocationLateActivityFailureRollsBackWholeV1Fold |
| 2.33s | parallel | provenance | TestRegisterSoftwareAgent |
| 2.32s | - | provenance/internal/sqlite | TestSuppressCheckConstraintsRestoresAndReportsBoth |
| 2.30s | - | provenance/internal/sqlite | TestFactQueryRejectsForgedSnapshotWatermark |
| 2.29s | serial | provenance | TestDBOSConditionFailureIsCheckpointedTypedAndPermanent |
| 2.28s | - | provenance/internal/sqlite | TestFactQueryContextFilterStaysScopedToFactRows |
| 2.27s | parallel | provenance | TestActivities |
| 2.27s | parallel | provenance | TestCreateEmptyNamespace |
| 2.26s | serial | provenance | TestDBOSApplyRejectsDuplicateStoredInputBeforeCallbacksOrWrites |
| 2.26s | parallel | provenance | TestRegisterHumanAgent |
| 2.25s | - | provenance/internal/sqlite | TestSingleActivationAttemptHonoursBusyTimeout |
| 2.25s | parallel | provenance | TestWithModelRegistry_EmptyRegistry |
| 2.22s | parallel | provenance | TestCloseTask |
| 2.21s | parallel | provenance | TestRegisterFixedSoftwareAgentManifestConflictIsActionableOnce |
| 2.20s | serial | provenance | TestFusedComposedBatchConditionIsCanonicalAtomicAndReplaySafe |
| 2.19s | parallel | provenance | TestShowNotFound |
| 2.17s | serial | provenance | TestFusedComposedReferenceScopeProvesDescendantAndRejectsUnrelated |
| 2.14s | parallel | provenance | TestRegisterSoftwareAgentRandomIDPathUnchanged |
| 2.14s | parallel | provenance | TestRegisterFixedSoftwareAgentReplayRepairAndDrift |
| 2.13s | serial | provenance | TestFusedGovernedAllocationParticipantErrorRollsBackDomainAuditAndSuccessfulCheckpoint |
| 2.12s | serial | provenance | TestCancel_AlreadyCancelled_StartsNothing |
| 2.12s | - | provenance/internal/sqlite | TestBorrowedReleaseRetiresConnectionWhenRestoreCannotLand |
| 2.12s | serial | provenance | TestCancel_WhileGated_DurableWorkContinues |
| 2.09s | serial | provenance | TestBorrowedTrackerCloseInvalidatesOnlyLocalTracker |
| 2.09s | parallel | provenance | TestWithModelRegistry_CustomRegistry |
| 2.07s | serial | provenance | TestComposedGovernedAllocationOperationShapeConflictsBeforeOwnerMarker |
| 2.04s | parallel | provenance | TestRegisterFixedSoftwareAgentOverlapIsActionableOnce |
| 2.03s | - | provenance/internal/sqlite | TestFactContextIntegrityRejectsStoredCorruption |
| 1.99s | serial | provenance | TestComposedGovernedAllocationReplayReceiptAndCopies |
| 1.98s | serial | provenance | TestDBOSAdapterReservedIdentityAdmission |
| 1.98s | serial | provenance | TestProductionSQLTextIsDecidableFromSource |
| 1.98s | serial | provenance | TestFusedGovernedAllocationParticipantReceivesDefensiveRequestAndClosureCopies |
| 1.95s | serial | provenance | TestFusedGovernedAllocationComposedPersistsAllowedSupplementsAndReplays |
| 1.93s | serial | provenance | TestFusedGovernedAllocationComposedConflictsAndDefensiveCopies |
| 1.91s | serial | provenance | TestNewDBOSAdapterRejectsInvalidConfigBeforeRegistration |
| 1.91s | serial | provenance | TestFusedGovernedAllocationComposedBatchCommitsOrderedCompleteClosureAndReplays |
| 1.88s | serial | provenance | TestFusedGovernedAllocationParticipantExactReplaySkipsCallbackAndDistinctWorkflowIsIdempotent |
| 1.86s | serial | provenance | TestComposedGovernedAllocationExactReceiptMissingOwnerMarkerIsCorruption |
| 1.85s | serial | provenance | TestFusedGovernedAllocationComposedBatchInvalidSecondChildWritesNothing |
| 1.85s | serial | provenance | TestComposedConflictProofRejectsMutatedAuthorityOwnerAndSupplement |
| 1.83s | serial | provenance | TestFreshWorkflowRejectsWrongParentAuthorityBeforeWrites |
| 1.83s | serial | provenance | TestDBOSAdapterTransferAssignmentChangedInputConflicts |
| 1.82s | serial | provenance | TestComposedParticipantFailureRollsBackEveryGovernedTable |
| 1.82s | serial | provenance | TestJoinedParticipantAndCleanupFailureCannotAuthenticateDomainRejection |
| 1.79s | - | provenance/internal/sqlite | TestFactContextStartupFailuresPreserveFiles |
| 1.79s | serial | provenance | TestGovernedAllocationComposedRejectsUnsupportedAndUnrelatedReferencesBeforeAllocation |
| 1.79s | serial | provenance | TestDBOSActivityConflictIsCheckpointedTypedAndActivityResultTransports |
| 1.78s | serial | provenance | TestFusedWorkflowIDReplayMatchesCanonicalRequestAndAuthority |
| 1.75s | serial | provenance | TestDBOSDurableSnapshotDetectsTaskAttributionMutation |
| 1.75s | serial | provenance | TestGovernedPublicIngressRejectsReservedOperationIDsBeforeDBOS |
| 1.73s | serial | provenance | TestComposedGovernedAllocationRejectsEmptyBeforeDBOS |
| 1.72s | - | provenance/internal/journal | TestMutationV1EveryResourceBoundaryIsExact |
| 1.72s | serial | provenance | TestRunInitializeRootRejectsReservedOperationIDBeforeDBOS |
| 1.67s | - | provenance/internal/helpers | TestAncestors_GraphTopologies |
| 1.67s | serial | provenance | TestSessionGovernedIngressRejectsReservedOperationIDsWithoutWrites |
| 1.64s | - | provenance/internal/helpers | TestDescendants_GraphTopologies |
| 1.51s | serial | provenance | TestMigrationColumnAddPathForColumnlessLegacyDB |
| 1.28s | serial | provenance | TestGovernedAllocationRejectsCyclicAncestryWithoutWrites |
| 1.24s | serial | provenance | TestGovernedGenesisRetryAndConflictingSecondGenesis |
| 1.20s | - | provenance/internal/sqlite | TestFactContextsPersistCanonicalReopenAndExactReplay |
| 1.20s | serial | provenance | TestSessionAllocationReplayRequiresExactAuthorityAndSurvivesLaterRevocation |
| 1.19s | serial | provenance | TestExternalAtomicJournalContractCompiles |
| 1.16s | serial | provenance | TestSessionTransferAssignmentExactReplayAfterLaterTransferAndReopen |
| 1.13s | serial | provenance | TestSessionAllocateGovernedComposedUsesSameReducer |
| 1.11s | serial | provenance | TestGovernedAllocationRejectsRevokedMiddleAncestorWithoutWrites |
| 1.11s | serial | provenance | TestGovernedAllocationRejectsRevocationWithoutWrites |
| 1.09s | - | provenance/internal/sqlite | TestBorrowedActivationWaitsOutConcurrentWriter |
| 1.08s | serial | provenance | TestSessionAllocateGovernedRejectsDifferentActiveParentAuthorityWithoutWrites |
| 1.06s | - | provenance/internal/sqlite | TestFileActivationReopensWithWALAndRuntimePragmas |
| 1.06s | serial | provenance | TestGovernedAllocationRejectsBeforeWriting |
| 1.04s | serial | provenance | TestSQLGuardRejectsEverySeededViolationClass |
| 1.03s | - | provenance/internal/sqlite | TestApplyContextBoundsContendedWriterByCallerDeadline |
| 1.03s | serial | provenance | TestTrackerConcurrentCloseIsIdempotent |
