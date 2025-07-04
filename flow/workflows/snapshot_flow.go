package peerflow

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/PeerDB-io/peerdb/flow/activities"
	"github.com/PeerDB-io/peerdb/flow/connectors/utils"
	"github.com/PeerDB-io/peerdb/flow/generated/protos"
	"github.com/PeerDB-io/peerdb/flow/internal"
	"github.com/PeerDB-io/peerdb/flow/shared"
)

type snapshotType int8

const (
	SNAPSHOT_TYPE_UNKNOWN snapshotType = iota
	SNAPSHOT_TYPE_SLOT
	SNAPSHOT_TYPE_TX
)

type SnapshotFlowExecution struct {
	config *protos.FlowConnectionConfigs
	logger log.Logger
}

func (s *SnapshotFlowExecution) setupReplication(
	ctx workflow.Context,
) (*protos.SetupReplicationOutput, error) {
	flowName := s.config.FlowJobName
	s.logger.Info("setting up replication on source for peer flow")

	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 4 * 24 * time.Hour,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
			MaximumAttempts: 20,
		},
	})

	tblNameMapping := make(map[string]string, len(s.config.TableMappings))
	for _, v := range s.config.TableMappings {
		tblNameMapping[v.SourceTableIdentifier] = v.DestinationTableIdentifier
	}

	setupReplicationInput := &protos.SetupReplicationInput{
		PeerName:                    s.config.SourceName,
		FlowJobName:                 flowName,
		TableNameMapping:            tblNameMapping,
		DoInitialSnapshot:           s.config.DoInitialSnapshot,
		ExistingPublicationName:     s.config.PublicationName,
		ExistingReplicationSlotName: s.config.ReplicationSlotName,
		Env:                         s.config.Env,
	}

	res := &protos.SetupReplicationOutput{}
	if err := workflow.ExecuteActivity(ctx, snapshot.SetupReplication, setupReplicationInput).Get(ctx, &res); err != nil {
		return nil, fmt.Errorf("failed to setup replication on source peer: %w", err)
	}

	s.logger.Info("replication slot live on source for peer flow")

	return res, nil
}

func (s *SnapshotFlowExecution) closeSlotKeepAlive(
	ctx workflow.Context,
) error {
	s.logger.Info("closing slot keep alive for peer flow")

	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 1 * time.Minute,
		},
	})

	if err := workflow.ExecuteActivity(ctx, snapshot.CloseSlotKeepAlive, s.config.FlowJobName).Get(ctx, nil); err != nil {
		return fmt.Errorf("failed to close slot keep alive for peer flow: %w", err)
	}

	s.logger.Info("closed slot keep alive for peer flow")

	return nil
}

func (s *SnapshotFlowExecution) cloneTable(
	ctx workflow.Context,
	boundSelector *shared.BoundSelector,
	snapshotName string,
	mapping *protos.TableMapping,
) error {
	flowName := s.config.FlowJobName
	cloneLog := slog.Group("clone-log",
		slog.String(string(shared.FlowNameKey), flowName),
		slog.String("snapshotName", snapshotName))

	srcName := mapping.SourceTableIdentifier
	dstName := mapping.DestinationTableIdentifier
	originalRunID := workflow.GetInfo(ctx).OriginalRunID

	childWorkflowID := fmt.Sprintf("clone_%s_%s_%s", flowName, srcName, originalRunID)
	childWorkflowID = shared.ReplaceIllegalCharactersWithUnderscores(childWorkflowID)

	s.logger.Info(fmt.Sprintf("Obtained child id %s for source table %s and destination table %s",
		childWorkflowID, srcName, dstName), cloneLog)

	taskQueue := internal.PeerFlowTaskQueueName(shared.PeerFlowTaskQueue)
	childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:          childWorkflowID,
		WorkflowTaskTimeout: 5 * time.Minute,
		TaskQueue:           taskQueue,
	})

	var tableSchema *protos.TableSchema
	initTableSchema := func() error {
		if tableSchema != nil {
			return nil
		}

		schemaCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: time.Minute,
			WaitForCancellation: true,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval: 1 * time.Minute,
			},
		})
		return workflow.ExecuteActivity(
			schemaCtx,
			snapshot.LoadTableSchema,
			s.config.FlowJobName,
			dstName,
		).Get(ctx, &tableSchema)
	}

	parsedSrcTable, err := utils.ParseSchemaTable(srcName)
	if err != nil {
		s.logger.Error("unable to parse source table", slog.Any("error", err), cloneLog)
		return fmt.Errorf("unable to parse source table: %w", err)
	}
	from := "*"
	if len(mapping.Exclude) != 0 {
		if err := initTableSchema(); err != nil {
			return err
		}
		quotedColumns := make([]string, 0, len(tableSchema.Columns))
		for _, col := range tableSchema.Columns {
			if !slices.Contains(mapping.Exclude, col.Name) {
				quotedColumns = append(quotedColumns, utils.QuoteIdentifier(col.Name))
			}
		}
		from = strings.Join(quotedColumns, ",")
	}

	// usually MySQL supports double quotes with ANSI_QUOTES, but Vitess doesn't
	// Vitess currently only supports initial load so change here is enough
	srcTableEscaped := parsedSrcTable.String()
	if dbtype, err := getPeerType(ctx, s.config.SourceName); err != nil {
		return err
	} else if dbtype == protos.DBType_MYSQL {
		srcTableEscaped = parsedSrcTable.MySQL()
	}

	var query string
	if mapping.PartitionKey == "" {
		query = fmt.Sprintf("SELECT %s FROM %s", from, srcTableEscaped)
	} else {
		query = fmt.Sprintf("SELECT %s FROM %s WHERE %s BETWEEN {{.start}} AND {{.end}}",
			from, srcTableEscaped, mapping.PartitionKey)
	}

	numWorkers := uint32(8)
	if s.config.SnapshotMaxParallelWorkers > 0 {
		numWorkers = s.config.SnapshotMaxParallelWorkers
	}

	numRowsPerPartition := uint32(250000)
	if s.config.SnapshotNumRowsPerPartition > 0 {
		numRowsPerPartition = s.config.SnapshotNumRowsPerPartition
	}

	snapshotWriteMode := &protos.QRepWriteMode{
		WriteType: protos.QRepWriteType_QREP_WRITE_MODE_APPEND,
	}

	// ensure document IDs are synchronized across initial load and CDC
	// for the same document
	if dbtype, err := getPeerType(ctx, s.config.DestinationName); err != nil {
		return err
	} else if dbtype == protos.DBType_ELASTICSEARCH {
		if err := initTableSchema(); err != nil {
			return err
		}
		snapshotWriteMode = &protos.QRepWriteMode{
			WriteType:        protos.QRepWriteType_QREP_WRITE_MODE_UPSERT,
			UpsertKeyColumns: tableSchema.PrimaryKeyColumns,
		}
	}

	config := &protos.QRepConfig{
		FlowJobName:                childWorkflowID,
		SourceName:                 s.config.SourceName,
		DestinationName:            s.config.DestinationName,
		Query:                      query,
		WatermarkColumn:            mapping.PartitionKey,
		WatermarkTable:             srcName,
		InitialCopyOnly:            true,
		SnapshotName:               snapshotName,
		DestinationTableIdentifier: dstName,
		NumRowsPerPartition:        numRowsPerPartition,
		MaxParallelWorkers:         numWorkers,
		StagingPath:                s.config.SnapshotStagingPath,
		SyncedAtColName:            s.config.SyncedAtColName,
		SoftDeleteColName:          s.config.SoftDeleteColName,
		WriteMode:                  snapshotWriteMode,
		System:                     s.config.System,
		Script:                     s.config.Script,
		Env:                        s.config.Env,
		ParentMirrorName:           flowName,
		Exclude:                    mapping.Exclude,
		Columns:                    mapping.Columns,
		Version:                    s.config.Version,
	}

	boundSelector.SpawnChild(childCtx, QRepFlowWorkflow, nil, config, nil)
	return nil
}

func (s *SnapshotFlowExecution) cloneTables(
	ctx workflow.Context,
	snapshotType snapshotType,
	slotName string,
	snapshotName string,
	supportsTIDScans bool,
	maxParallelClones int,
) error {
	if snapshotType == SNAPSHOT_TYPE_SLOT {
		s.logger.Info("cloning tables for slot", slog.String("slot", slotName), slog.String("snapshot", snapshotName))
	} else if snapshotType == SNAPSHOT_TYPE_TX {
		s.logger.Info("cloning tables in tx snapshot mode", slog.String("snapshot", snapshotName))
	}

	boundSelector := shared.NewBoundSelector(ctx, "CloneTablesSelector", maxParallelClones)

	defaultPartitionCol := "ctid"
	if !supportsTIDScans {
		s.logger.Info("Postgres version too old for TID scans, might use full table partitions!")
		defaultPartitionCol = ""
	}

	for _, v := range s.config.TableMappings {
		source := v.SourceTableIdentifier
		destination := v.DestinationTableIdentifier
		s.logger.Info(
			fmt.Sprintf("Cloning table with source table %s and destination table name %s", source, destination),
			slog.String("snapshotName", snapshotName),
		)
		if v.PartitionKey == "" {
			v.PartitionKey = defaultPartitionCol
		}
		if err := s.cloneTable(ctx, boundSelector, snapshotName, v); err != nil {
			s.logger.Error("failed to start clone child workflow", slog.Any("error", err))
			continue
		}
	}

	if err := boundSelector.Wait(ctx); err != nil {
		s.logger.Error("failed to clone some tables", "error", err)
		return err
	}

	s.logger.Info("finished cloning tables")
	return nil
}

func (s *SnapshotFlowExecution) cloneTablesWithSlot(
	ctx workflow.Context,
	sessionCtx workflow.Context,
	numTablesInParallel int,
) error {
	slotInfo, err := s.setupReplication(sessionCtx)
	if err != nil {
		return fmt.Errorf("failed to setup replication: %w", err)
	}
	defer func() {
		dCtx, cancel := workflow.NewDisconnectedContext(sessionCtx)
		defer cancel()
		if err := s.closeSlotKeepAlive(dCtx); err != nil {
			s.logger.Error("failed to close slot keep alive", slog.Any("error", err))
		}
	}()

	s.logger.Info(fmt.Sprintf("cloning %d tables in parallel", numTablesInParallel))
	if err := s.cloneTables(ctx,
		SNAPSHOT_TYPE_SLOT,
		slotInfo.SlotName,
		slotInfo.SnapshotName,
		slotInfo.SupportsTidScans,
		numTablesInParallel,
	); err != nil {
		s.logger.Error("failed to clone tables", slog.Any("error", err))
		return fmt.Errorf("failed to clone tables: %w", err)
	}

	return nil
}

func SnapshotFlowWorkflow(
	ctx workflow.Context,
	config *protos.FlowConnectionConfigs,
) error {
	se := &SnapshotFlowExecution{
		config: config,
		logger: log.With(workflow.GetLogger(ctx),
			slog.String(string(shared.FlowNameKey), config.FlowJobName),
			slog.String("sourcePeer", config.SourceName)),
	}

	numTablesInParallel := int(max(config.SnapshotNumTablesInParallel, 1))

	sessionOpts := &workflow.SessionOptions{
		CreationTimeout:  5 * time.Minute,
		ExecutionTimeout: time.Hour * 24 * 365 * 100, // 100 years
		HeartbeatTimeout: time.Hour,
	}
	sessionCtx, err := workflow.CreateSession(ctx, sessionOpts)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer workflow.CompleteSession(sessionCtx)

	if !config.DoInitialSnapshot {
		if _, err := se.setupReplication(sessionCtx); err != nil {
			return fmt.Errorf("failed to setup replication: %w", err)
		}

		if err := se.closeSlotKeepAlive(sessionCtx); err != nil {
			return fmt.Errorf("failed to close slot keep alive: %w", err)
		}

		return nil
	}

	if config.InitialSnapshotOnly {
		sessionInfo := workflow.GetSessionInfo(sessionCtx)

		exportCtx := workflow.WithActivityOptions(sessionCtx, workflow.ActivityOptions{
			StartToCloseTimeout: sessionOpts.ExecutionTimeout,
			HeartbeatTimeout:    10 * time.Minute,
			WaitForCancellation: true,
			RetryPolicy: &temporal.RetryPolicy{
				InitialInterval: 1 * time.Minute,
			},
		})

		fMaintain := workflow.ExecuteActivity(
			exportCtx,
			snapshot.MaintainTx,
			sessionInfo.SessionID,
			config.SourceName,
			config.Env,
		)

		fExportSnapshot := workflow.ExecuteActivity(
			exportCtx,
			snapshot.WaitForExportSnapshot,
			sessionInfo.SessionID,
		)

		var sessionError error
		var txnSnapshotState *activities.TxSnapshotState
		sessionSelector := workflow.NewNamedSelector(ctx, "ExportSnapshotSetup")
		sessionSelector.AddFuture(fMaintain, func(f workflow.Future) {
			// MaintainTx should never exit without an error before this point
			sessionError = f.Get(exportCtx, nil)
		})
		sessionSelector.AddFuture(fExportSnapshot, func(f workflow.Future) {
			// Happy path is waiting for this to return without error
			sessionError = f.Get(exportCtx, &txnSnapshotState)
		})
		sessionSelector.AddReceive(ctx.Done(), func(_ workflow.ReceiveChannel, _ bool) {
			sessionError = ctx.Err()
		})
		sessionSelector.Select(ctx)
		if sessionError != nil {
			return sessionError
		}

		if err := se.cloneTables(ctx,
			SNAPSHOT_TYPE_TX,
			"",
			txnSnapshotState.SnapshotName,
			txnSnapshotState.SupportsTIDScans,
			numTablesInParallel,
		); err != nil {
			return fmt.Errorf("failed to clone tables: %w", err)
		}
	} else if err := se.cloneTablesWithSlot(ctx, sessionCtx, numTablesInParallel); err != nil {
		return fmt.Errorf("failed to clone slots and create replication slot: %w", err)
	}

	return nil
}
