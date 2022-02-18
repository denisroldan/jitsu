package storages

import (
	"context"
	"errors"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"github.com/jitsucom/jitsu/server/adapters"
	"github.com/jitsucom/jitsu/server/events"
	"github.com/jitsucom/jitsu/server/logging"
	"github.com/jitsucom/jitsu/server/schema"
	"github.com/jitsucom/jitsu/server/timestamp"
	"github.com/jitsucom/jitsu/server/typing"
	sf "github.com/snowflakedb/gosnowflake"
)

//Snowflake stores files to Snowflake in two modes:
//batch: via aws s3 (or gcp) in batch mode (1 file = 1 transaction)
//stream: via events queue in stream mode (1 object = 1 transaction)
type Snowflake struct {
	Abstract

	stageAdapter                  adapters.Stage
	snowflakeAdapter              *adapters.Snowflake
	usersRecognitionConfiguration *UserRecognitionConfiguration
}

func init() {
	RegisterStorage(StorageType{typeName: SnowflakeType, createFunc: NewSnowflake, isSQL: true})
}

//NewSnowflake returns Snowflake and start goroutine for Snowflake batch storage or for stream consumer depend on destination mode
func NewSnowflake(config *Config) (storage Storage, err error) {
	defer func() {
		if err != nil && storage != nil {
			storage.Close()
			storage = nil
		}
	}()
	snowflakeConfig := &adapters.SnowflakeConfig{}
	if err = config.destination.GetDestConfig(config.destination.Snowflake, snowflakeConfig); err != nil {
		return
	}
	if snowflakeConfig.Schema == "" {
		snowflakeConfig.Schema = "PUBLIC"
		logging.Warnf("[%s] schema wasn't provided. Will be used default one: %s", config.destinationID, snowflakeConfig.Schema)
	}

	//default client_session_keep_alive
	if _, ok := snowflakeConfig.Parameters["client_session_keep_alive"]; !ok {
		t := "true"
		snowflakeConfig.Parameters["client_session_keep_alive"] = &t
	}
	var googleConfig *adapters.GoogleConfig
	gc, err := config.destination.GetConfig(snowflakeConfig.Google, config.destination.Google, &adapters.GoogleConfig{})
	if err != nil {
		return
	}
	googleConfig, googleOk := gc.(*adapters.GoogleConfig)
	if googleOk {
		if err = googleConfig.Validate(); err != nil {
			return
		}
		if !config.streamMode {
			if err = googleConfig.ValidateBatchMode(); err != nil {
				return
			}
		}

		//stage is required when gcp integration
		if snowflakeConfig.Stage == "" {
			return nil, errors.New("Snowflake stage is required parameter in GCP integration")
		}
	}

	var stageAdapter adapters.Stage
	var s3config *adapters.S3Config
	s3c, err := config.destination.GetConfig(snowflakeConfig.S3, config.destination.S3, &adapters.S3Config{})
	if err != nil {
		return
	}
	s3config, s3ok := s3c.(*adapters.S3Config)
	if !config.streamMode {
		if s3ok {
			stageAdapter, err = adapters.NewS3(s3config)
			if err != nil {
				return
			}
		} else {
			stageAdapter, err = adapters.NewGoogleCloudStorage(config.ctx, googleConfig)
			if err != nil {
				return
			}
		}
	}
	snowflake := &Snowflake{stageAdapter: stageAdapter}
	err = snowflake.Init(config)
	if err != nil {
		return
	}
	storage = snowflake
	queryLogger := config.loggerFactory.CreateSQLQueryLogger(config.destinationID)
	snowflakeAdapter, err := CreateSnowflakeAdapter(config.ctx, s3config, *snowflakeConfig, queryLogger, snowflake.sqlTypes)
	if err != nil {
		return
	}

	tableHelper := NewTableHelper(snowflakeConfig.Schema, snowflakeAdapter, config.coordinationService, config.pkFields, adapters.SchemaToSnowflake, config.maxColumns, SnowflakeType)

	snowflake.snowflakeAdapter = snowflakeAdapter
	snowflake.usersRecognitionConfiguration = config.usersRecognition

	//Abstract
	snowflake.tableHelpers = []*TableHelper{tableHelper}
	snowflake.sqlAdapters = []adapters.SQLAdapter{snowflakeAdapter}

	//streaming worker (queue reading)
	snowflake.streamingWorker = newStreamingWorker(config.eventQueue, snowflake, tableHelper)
	return
}

//CreateSnowflakeAdapter creates snowflake adapter with schema
//if schema doesn't exist - snowflake returns error. In this case connect without schema and create it
func CreateSnowflakeAdapter(ctx context.Context, s3Config *adapters.S3Config, config adapters.SnowflakeConfig,
	queryLogger *logging.QueryLogger, sqlTypes typing.SQLTypes) (*adapters.Snowflake, error) {
	snowflakeAdapter, err := adapters.NewSnowflake(ctx, &config, s3Config, queryLogger, sqlTypes)
	if err != nil {
		if sferr, ok := err.(*sf.SnowflakeError); ok {
			//schema doesn't exist
			if sferr.Number == sf.ErrObjectNotExistOrAuthorized {
				snowflakeSchema := config.Schema
				config.Schema = ""
				//create adapter without a certain schema
				tmpSnowflakeAdapter, err := adapters.NewSnowflake(ctx, &config, s3Config, queryLogger, sqlTypes)
				if err != nil {
					return nil, err
				}
				defer tmpSnowflakeAdapter.Close()

				config.Schema = snowflakeSchema
				//create schema and reconnect
				if err = tmpSnowflakeAdapter.CreateDbSchema(config.Schema); err != nil {
					return nil, err
				}

				//create adapter with a certain schema
				snowflakeAdapterWithSchema, err := adapters.NewSnowflake(ctx, &config, s3Config, queryLogger, sqlTypes)
				if err != nil {
					return nil, err
				}
				return snowflakeAdapterWithSchema, nil
			}
		}
		return nil, err
	}
	return snowflakeAdapter, nil
}

//Store process events and stores with storeTable() func
//returns store result per table, failed events (group of events which are failed to process) and err
func (s *Snowflake) Store(fileName string, objects []map[string]interface{}, alreadyUploadedTables map[string]bool, needCopyEvent bool) (map[string]*StoreResult, *events.FailedEvents, *events.SkippedEvents, error) {
	_, tableHelper := s.getAdapters()
	flatData, failedEvents, skippedEvents, err := s.processor.ProcessEvents(fileName, objects, alreadyUploadedTables, needCopyEvent)
	if err != nil {
		return nil, nil, nil, err
	}

	//update cache with failed events
	for _, failedEvent := range failedEvents.Events {
		s.eventsCache.Error(s.IsCachingDisabled(), s.ID(), string(failedEvent.Event), failedEvent.Error)
	}
	//update cache and counter with skipped events
	for _, skipEvent := range skippedEvents.Events {
		s.eventsCache.Skip(s.IsCachingDisabled(), s.ID(), string(skipEvent.Event), skipEvent.Error)
	}

	storeFailedEvents := true
	tableResults := map[string]*StoreResult{}
	for _, fdata := range flatData {
		table := tableHelper.MapTableSchema(fdata.BatchHeader)
		err := s.storeTable(fdata, table)
		tableResults[table.Name] = &StoreResult{Err: err, RowsCount: fdata.GetPayloadLen(), EventsSrc: fdata.GetEventsPerSrc()}
		if err != nil {
			storeFailedEvents = false
		}

		//events cache
		writeEventsToCache(s, s.eventsCache, table, fdata, err)
	}

	//store failed events to fallback only if other events have been inserted ok
	if storeFailedEvents {
		return tableResults, failedEvents, skippedEvents, nil
	}

	return tableResults, nil, skippedEvents, nil
}

//check table schema
//and store data into one table via stage (google cloud storage or s3)
func (s *Snowflake) storeTable(fdata *schema.ProcessedFile, table *adapters.Table) error {
	_, tableHelper := s.getAdapters()
	dbTable, err := tableHelper.EnsureTableWithoutCaching(s.ID(), table)
	if err != nil {
		return err
	}

	b, header := fdata.GetPayloadBytesWithHeader(schema.VerticalBarSeparatedMarshallerInstance)
	if err := s.stageAdapter.UploadBytes(fdata.FileName, b); err != nil {
		return err
	}

	if err := s.snowflakeAdapter.Copy(fdata.FileName, dbTable.Name, header); err != nil {
		return fmt.Errorf("Error copying file [%s] from stage to snowflake: %v", fdata.FileName, err)
	}

	if err := s.stageAdapter.DeleteObject(fdata.FileName); err != nil {
		logging.SystemErrorf("[%s] file %s wasn't deleted from stage: %v", s.ID(), fdata.FileName, err)
	}

	return nil
}

//GetUsersRecognition returns users recognition configuration
func (s *Snowflake) GetUsersRecognition() *UserRecognitionConfiguration {
	return s.usersRecognitionConfiguration
}

// SyncStore is used in storing chunk of pulled data to Snowflake with processing
func (s *Snowflake) SyncStore(overriddenDataSchema *schema.BatchHeader, objects []map[string]interface{}, timeIntervalValue string, cacheTable bool, needCopyEvent bool) error {
	return syncStoreImpl(s, overriddenDataSchema, objects, timeIntervalValue, cacheTable, needCopyEvent)
}

func (s *Snowflake) Clean(tableName string) error {
	return cleanImpl(s, tableName)
}

//Update updates record in Snowflake
func (s *Snowflake) Update(object map[string]interface{}) error {
	_, tableHelper := s.getAdapters()
	envelops, err := s.processor.ProcessEvent(object, false)
	if err != nil {
		return err
	}
	for _, envelop := range envelops {
		batchHeader := envelop.Header
		processedObject := envelop.Event
		table := tableHelper.MapTableSchema(batchHeader)

		dbSchema, err := tableHelper.EnsureTableWithCaching(s.ID(), table)
		if err != nil {
			return err
		}

		start := timestamp.Now()
		if err = s.snowflakeAdapter.Update(dbSchema, processedObject, s.uniqueIDField.GetFlatFieldName(), s.uniqueIDField.Extract(object)); err != nil {
			return err
		}

		logging.Debugf("[%s] Updated 1 row in [%.2f] seconds", s.ID(), timestamp.Now().Sub(start).Seconds())
	}

	return nil
}

//Type returns Snowflake type
func (s *Snowflake) Type() string {
	return SnowflakeType
}

//Close closes Snowflake adapter, stage adapter, fallback logger and streaming worker
func (s *Snowflake) Close() (multiErr error) {
	if s.streamingWorker != nil {
		if err := s.streamingWorker.Close(); err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("[%s] Error closing streaming worker: %v", s.ID(), err))
		}
	}
	if s.snowflakeAdapter != nil {
		if err := s.snowflakeAdapter.Close(); err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("[%s] Error closing snowflake datasource: %v", s.ID(), err))
		}
	}
	if s.stageAdapter != nil {
		if err := s.stageAdapter.Close(); err != nil {
			multiErr = multierror.Append(multiErr, fmt.Errorf("[%s] Error closing snowflake stage: %v", s.ID(), err))
		}
	}

	if err := s.close(); err != nil {
		multiErr = multierror.Append(multiErr, err)
	}

	return
}
