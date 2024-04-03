package v1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"log/slog"

	"github.com/alexmullins/zip"
	"github.com/google/cel-go/cel"
	"github.com/labstack/echo/v4"
	"github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/xuri/excelize/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bytebase/bytebase/backend/store/model"

	"github.com/bytebase/bytebase/backend/common"
	"github.com/bytebase/bytebase/backend/common/log"
	"github.com/bytebase/bytebase/backend/component/activity"
	"github.com/bytebase/bytebase/backend/component/config"
	"github.com/bytebase/bytebase/backend/component/dbfactory"
	"github.com/bytebase/bytebase/backend/component/iam"
	"github.com/bytebase/bytebase/backend/component/masker"
	enterprise "github.com/bytebase/bytebase/backend/enterprise/api"
	api "github.com/bytebase/bytebase/backend/legacyapi"
	"github.com/bytebase/bytebase/backend/plugin/advisor"
	"github.com/bytebase/bytebase/backend/plugin/advisor/catalog"
	"github.com/bytebase/bytebase/backend/plugin/db"
	"github.com/bytebase/bytebase/backend/plugin/parser/base"
	mapperparser "github.com/bytebase/bytebase/backend/plugin/parser/mybatis/mapper"
	"github.com/bytebase/bytebase/backend/plugin/parser/sql/transform"
	"github.com/bytebase/bytebase/backend/plugin/schema"
	"github.com/bytebase/bytebase/backend/runner/schemasync"
	"github.com/bytebase/bytebase/backend/store"
	"github.com/bytebase/bytebase/backend/utils"
	storepb "github.com/bytebase/bytebase/proto/generated-go/store"
	v1pb "github.com/bytebase/bytebase/proto/generated-go/v1"
)

const (
	// The maximum number of bytes for sql results in response body.
	// 100 MB.
	maximumSQLResultSize = 100 * 1024 * 1024
	// defaultTimeout is the default timeout for query and admin execution.
	defaultTimeout = 10 * time.Minute
)

// SQLService is the service for SQL.
type SQLService struct {
	v1pb.UnimplementedSQLServiceServer
	store           *store.Store
	schemaSyncer    *schemasync.Syncer
	dbFactory       *dbfactory.DBFactory
	activityManager *activity.Manager
	licenseService  enterprise.LicenseService
	profile         *config.Profile
	iamManager      *iam.Manager
}

// NewSQLService creates a SQLService.
func NewSQLService(
	store *store.Store,
	schemaSyncer *schemasync.Syncer,
	dbFactory *dbfactory.DBFactory,
	activityManager *activity.Manager,
	licenseService enterprise.LicenseService,
	profile *config.Profile,
	iamManager *iam.Manager,
) *SQLService {
	return &SQLService{
		store:           store,
		schemaSyncer:    schemaSyncer,
		dbFactory:       dbFactory,
		activityManager: activityManager,
		licenseService:  licenseService,
		profile:         profile,
		iamManager:      iamManager,
	}
}

type maskingPolicyKey struct {
	schema string
	table  string
	column string
}

// Pretty returns pretty format SDL.
func (*SQLService) Pretty(_ context.Context, request *v1pb.PrettyRequest) (*v1pb.PrettyResponse, error) {
	engine := convertEngine(request.Engine)
	if _, err := transform.CheckFormat(engine, request.ExpectedSchema); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "User SDL is not SDL format: %s", err.Error())
	}
	if _, err := transform.CheckFormat(engine, request.CurrentSchema); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Dumped SDL is not SDL format: %s", err.Error())
	}

	prettyExpectedSchema, err := transform.SchemaTransform(engine, request.ExpectedSchema)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to transform user SDL: %s", err.Error())
	}
	prettyCurrentSchema, err := transform.Normalize(engine, request.CurrentSchema, prettyExpectedSchema)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to normalize dumped SDL: %s", err.Error())
	}

	return &v1pb.PrettyResponse{
		CurrentSchema:  prettyCurrentSchema,
		ExpectedSchema: prettyExpectedSchema,
	}, nil
}

// AdminExecute executes the SQL statement.
func (s *SQLService) AdminExecute(server v1pb.SQLService_AdminExecuteServer) error {
	ctx := server.Context()
	var driver db.Driver
	var conn *sql.Conn
	defer func() {
		if conn != nil {
			if err := conn.Close(); err != nil {
				slog.Warn("failed to close connection", log.BBError(err))
			}
		}
		if driver != nil {
			driver.Close(ctx)
		}
	}()
	for {
		request, err := server.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return status.Errorf(codes.Internal, "failed to receive request: %v", err)
		}

		instance, database, activity, err := s.preAdminExecute(ctx, request)
		if err != nil {
			return err
		}

		// We only need to get the driver and connection once.
		if driver == nil {
			driver, err = s.dbFactory.GetAdminDatabaseDriver(ctx, instance, database, db.ConnectionContext{})
			if err != nil {
				return status.Errorf(codes.Internal, "failed to get database driver: %v", err)
			}

			sqlDB := driver.GetDB()
			if sqlDB != nil {
				conn, err = sqlDB.Conn(ctx)
				if err != nil {
					return status.Errorf(codes.Internal, "failed to get database connection: %v", err)
				}
			}
		}

		result, durationNs, queryErr := s.doAdminExecute(ctx, driver, conn, request)
		sanitizeResults(result)

		if err := s.postQuery(ctx, database, activity, durationNs, queryErr); err != nil {
			slog.Error("failed to post admin execute activity", log.BBError(err))
		}

		response := &v1pb.AdminExecuteResponse{}
		if queryErr != nil {
			response.Results = []*v1pb.QueryResult{
				{
					Error: queryErr.Error(),
				},
			}
		} else {
			response.Results = result
		}

		if proto.Size(response) > maximumSQLResultSize {
			response.Results = []*v1pb.QueryResult{
				{
					Error: fmt.Sprintf("Output of query exceeds max allowed output size of %dMB", maximumSQLResultSize/1024/1024),
				},
			}
		}

		if err := server.Send(response); err != nil {
			return status.Errorf(codes.Internal, "failed to send response: %v", err)
		}
	}
}

func (*SQLService) doAdminExecute(ctx context.Context, driver db.Driver, conn *sql.Conn, request *v1pb.AdminExecuteRequest) ([]*v1pb.QueryResult, int64, error) {
	start := time.Now().UnixNano()
	timeout := defaultTimeout
	if request.Timeout != nil {
		timeout = request.Timeout.AsDuration()
	}
	ctx, cancelCtx := context.WithTimeout(ctx, timeout)
	defer cancelCtx()
	result, err := driver.RunStatement(ctx, conn, request.Statement)
	select {
	case <-ctx.Done():
		// canceled or timed out
		return nil, time.Now().UnixNano() - start, errors.Errorf("timeout reached: %v", timeout)
	default:
		// So the select will not block
	}
	return result, time.Now().UnixNano() - start, err
}

func (s *SQLService) preAdminExecute(ctx context.Context, request *v1pb.AdminExecuteRequest) (*store.InstanceMessage, *store.DatabaseMessage, *store.ActivityMessage, error) {
	user, _, instance, database, err := s.prepareRelatedMessage(ctx, request.Name, request.ConnectionDatabase)
	if err != nil {
		return nil, nil, nil, err
	}
	activity, err := s.createQueryActivity(ctx, user, api.ActivityInfo, instance.UID, database, api.ActivitySQLEditorQueryPayload{
		Statement:    request.Statement,
		InstanceID:   instance.UID,
		DatabaseID:   database.UID,
		DatabaseName: database.DatabaseName,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	return instance, database, activity, nil
}

// Export exports the SQL query result.
func (s *SQLService) Export(ctx context.Context, request *v1pb.ExportRequest) (*v1pb.ExportResponse, error) {
	// Prehandle the issue export.
	if strings.Contains(request.Name, common.IssueNamePrefix) {
		return s.DataExportIssueExport(ctx, request.Name)
	}
	// Prepare related message.
	user, environment, instance, database, err := s.prepareRelatedMessage(ctx, request.Name, request.ConnectionDatabase)
	if err != nil {
		return nil, err
	}

	statement := request.Statement
	// In Redshift datashare, Rewrite query used for parser.
	if database.DataShare {
		statement = strings.ReplaceAll(statement, fmt.Sprintf("%s.", database.DatabaseName), "")
	}

	// Validate the request.
	if err := validateQueryRequest(instance, statement); err != nil {
		return nil, err
	}

	schemaName := ""
	if instance.Engine == storepb.Engine_ORACLE {
		// For Oracle, there are two modes, schema-based and database-based management.
		// For schema-based management, also say tenant mode, we need to use the schemaName as the databaseName.
		// So the default schemaName is the database name.
		// For database-based management, we need to use the dataSource.Username as the schemaName.
		// So the default schemaName is the dataSource.Username.
		isSchemaTenantMode := (instance.Options != nil && instance.Options.GetSchemaTenantMode())
		if isSchemaTenantMode {
			schemaName = database.DatabaseName
		} else {
			dataSource, _, err := s.dbFactory.GetReadOnlyDatabaseSource(instance, database, "" /* dataSourceID */)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to get read only database source: %v", err.Error())
			}
			schemaName = dataSource.Username
		}
	}

	spans, err := base.GetQuerySpan(
		ctx,
		instance.Engine,
		statement,
		database.DatabaseName,
		schemaName,
		BuildGetDatabaseMetadataFunc(s.store, instance, database.DatabaseName),
		BuildListDatabaseNamesFunc(s.store, instance),
		store.IgnoreDatabaseAndTableCaseSensitive(instance),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get query span: %v", err.Error())
	}

	if s.licenseService.IsFeatureEnabled(api.FeatureAccessControl) == nil {
		if err := s.accessCheck(ctx, instance, user, spans, request.Limit, false /* isAdmin */, true /* isExport */); err != nil {
			return nil, err
		}
	}

	// Run SQL review.
	if _, _, err = s.sqlReviewCheck(ctx, statement, v1pb.CheckRequest_CHANGE_TYPE_UNSPECIFIED, environment, instance, database, nil /* Override Metadata */); err != nil {
		return nil, err
	}

	// Create export activity.
	activity, err := s.createExportActivity(ctx, user, api.ActivityInfo, instance.UID, database, api.ActivitySQLExportPayload{
		Statement:    request.Statement,
		InstanceID:   instance.UID,
		DatabaseID:   database.UID,
		DatabaseName: database.DatabaseName,
	})
	if err != nil {
		return nil, err
	}

	bytes, durationNs, exportErr := DoExport(ctx, s.store, s.dbFactory, s.licenseService, request, instance, database, spans)

	if err := s.postExport(ctx, database, activity, durationNs, exportErr); err != nil {
		return nil, err
	}

	if exportErr != nil {
		return nil, status.Errorf(codes.Internal, exportErr.Error())
	}

	content, err := DoEncrypt(bytes, request)
	if err != nil {
		return nil, status.Errorf(codes.Internal, err.Error())
	}

	return &v1pb.ExportResponse{
		Content: content,
	}, nil
}

func (s *SQLService) DataExportIssueExport(ctx context.Context, issueName string) (*v1pb.ExportResponse, error) {
	issueUID, err := common.GetIssueID(issueName)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to get issue ID: %v", err)
	}
	issue, err := s.store.GetIssueV2(ctx, &store.FindIssueMessage{UID: &issueUID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get issue: %v", err)
	}
	if issue.PipelineUID == nil {
		return nil, status.Errorf(codes.InvalidArgument, "issue %s has no pipeline", issueName)
	}
	rollout, err := s.store.GetRollout(ctx, *issue.PipelineUID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get rollout: %v", err)
	}
	tasks, err := s.store.ListTasks(ctx, &api.TaskFind{PipelineID: &rollout.ID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get tasks: %v", err)
	}
	if len(tasks) != 1 {
		return nil, status.Errorf(codes.InvalidArgument, "issue %s has unmatched tasks", issueName)
	}
	task := tasks[0]
	taskRuns, err := s.store.ListTaskRunsV2(ctx, &store.FindTaskRunMessage{TaskUID: &task.ID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get task run: %v", err)
	}
	if len(taskRuns) == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "issue %s has no task run", issueName)
	}
	taskRun := taskRuns[len(taskRuns)-1]
	exportArchiveUID := int(taskRun.ResultProto.ExportArchiveUid)
	if exportArchiveUID == 0 {
		return nil, status.Errorf(codes.InvalidArgument, "issue %s has no export archive", issueName)
	}
	exportArchive, err := s.store.GetExportArchive(ctx, &store.FindExportArchiveMessage{UID: &exportArchiveUID})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get export archive: %v", err)
	}
	if exportArchive == nil {
		return nil, status.Errorf(codes.NotFound, "export archive %d not found", exportArchiveUID)
	}
	// Delete the export archive after it's fetched.
	if err := s.store.DeleteExportArchive(ctx, exportArchiveUID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to delete export archive: %v", err)
	}
	return &v1pb.ExportResponse{
		Content: exportArchive.Bytes,
	}, nil
}

// DoExport does the export.
func DoExport(ctx context.Context, storeInstance *store.Store, dbFactory *dbfactory.DBFactory, licenseService enterprise.LicenseService, request *v1pb.ExportRequest, instance *store.InstanceMessage, database *store.DatabaseMessage, spans []*base.QuerySpan) ([]byte, int64, error) {
	driver, err := dbFactory.GetReadOnlyDatabaseDriver(ctx, instance, database, "" /* dataSourceID */)
	if err != nil {
		return nil, 0, err
	}
	defer driver.Close(ctx)

	sqlDB := driver.GetDB()
	var conn *sql.Conn
	if sqlDB != nil {
		conn, err = sqlDB.Conn(ctx)
		if err != nil {
			return nil, 0, err
		}
		defer conn.Close()
	}

	queryContext := &db.QueryContext{
		ReadOnly:            true,
		CurrentDatabase:     database.DatabaseName,
		SensitiveSchemaInfo: nil,
		EnableSensitive:     licenseService.IsFeatureEnabledForInstance(api.FeatureSensitiveData, instance) == nil,
	}
	if request.Limit != 0 {
		queryContext.Limit = int(request.Limit)
	}
	start := time.Now().UnixNano()
	result, err := driver.QueryConn(ctx, conn, request.Statement, queryContext)
	durationNs := time.Now().UnixNano() - start
	if err != nil {
		return nil, durationNs, err
	}
	// only return the last result
	if len(result) > 1 {
		result = result[len(result)-1:]
	}
	if proto.Size(&v1pb.QueryResponse{Results: result}) > maximumSQLResultSize {
		return nil, durationNs, errors.Errorf("Output of query exceeds max allowed output size of %dMB", maximumSQLResultSize/1024/1024)
	}

	if licenseService.IsFeatureEnabledForInstance(api.FeatureSensitiveData, instance) == nil {
		masker := NewQueryResultMasker(storeInstance)
		if err := masker.MaskResults(ctx, spans, result, instance, storepb.MaskingExceptionPolicy_MaskingException_EXPORT); err != nil {
			return nil, durationNs, err
		}
	}

	var content []byte
	switch request.Format {
	case v1pb.ExportFormat_CSV:
		if content, err = exportCSV(result[0]); err != nil {
			return nil, durationNs, err
		}
	case v1pb.ExportFormat_JSON:
		if content, err = exportJSON(result[0]); err != nil {
			return nil, durationNs, err
		}
	case v1pb.ExportFormat_SQL:
		resourceList, err := extractResourceList(ctx, storeInstance, instance.Engine, database.DatabaseName, request.Statement, instance)
		if err != nil {
			return nil, 0, status.Errorf(codes.InvalidArgument, "failed to extract resource list: %v", err)
		}
		statementPrefix, err := getSQLStatementPrefix(instance.Engine, resourceList, result[0].ColumnNames)
		if err != nil {
			return nil, 0, err
		}
		if content, err = exportSQL(instance.Engine, statementPrefix, result[0]); err != nil {
			return nil, durationNs, err
		}
	case v1pb.ExportFormat_XLSX:
		if content, err = exportXLSX(result[0]); err != nil {
			return nil, durationNs, err
		}
	default:
		return nil, durationNs, status.Errorf(codes.InvalidArgument, "unsupported export format: %s", request.Format.String())
	}
	return content, durationNs, nil
}

func (s *SQLService) postExport(ctx context.Context, database *store.DatabaseMessage, activity *store.ActivityMessage, durationNs int64, queryErr error) error {
	// Update the activity
	var payload api.ActivitySQLExportPayload
	if err := json.Unmarshal([]byte(activity.Payload), &payload); err != nil {
		return status.Errorf(codes.Internal, "failed to unmarshal activity payload: %v", err)
	}

	var newLevel *api.ActivityLevel
	payload.DurationNs = durationNs
	if queryErr != nil {
		payload.Error = queryErr.Error()
		errorLevel := api.ActivityError
		newLevel = &errorLevel
	}

	// TODO: update the advice list

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("Failed to marshal activity after exporting sql statement",
			slog.String("database_name", payload.DatabaseName),
			slog.Int("instance_id", payload.InstanceID),
			slog.String("statement", payload.Statement),
			log.BBError(err))
		return status.Errorf(codes.Internal, "Failed to marshal activity after exporting sql statement: %v", err)
	}

	payloadString := string(payloadBytes)
	if _, err := s.store.UpdateActivityV2(ctx, &store.UpdateActivityMessage{
		UID:     activity.UID,
		Level:   newLevel,
		Payload: &payloadString,
	}); err != nil {
		return status.Errorf(codes.Internal, "Failed to update activity after exporting sql statement: %v", err)
	}

	if _, err := s.store.CreateQueryHistory(ctx, &store.QueryHistoryMessage{
		CreatorUID: activity.CreatorUID,
		ProjectID:  database.ProjectID,
		Database:   common.FormatDatabase(database.InstanceID, database.DatabaseName),
		Statement:  payload.Statement,
		Type:       store.QueryHistoryTypeExport,
		Payload: &storepb.QueryHistoryPayload{
			Error:    &payload.Error,
			Duration: durationpb.New(time.Duration(durationNs)),
		},
	}); err != nil {
		return status.Errorf(codes.Internal, "Failed to create export history with error: %v", err)
	}

	return nil
}

func DoEncrypt(data []byte, request *v1pb.ExportRequest) ([]byte, error) {
	if request.Password == "" {
		return data, nil
	}
	var b bytes.Buffer
	fzip := io.Writer(&b)

	zipw := zip.NewWriter(fzip)
	defer zipw.Close()

	filename := fmt.Sprintf("export.%s", strings.ToLower(request.Format.String()))

	writer, err := zipw.Encrypt(filename, request.Password)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create encrypt export file")
	}

	if _, err := io.Copy(writer, bytes.NewReader(data)); err != nil {
		return nil, errors.Wrapf(err, "failed to write export file")
	}
	if err := zipw.Close(); err != nil {
		return nil, errors.Wrap(err, "failed to close zip writer")
	}

	return b.Bytes(), nil
}

func (*SQLService) StringifyMetadata(_ context.Context, request *v1pb.StringifyMetadataRequest) (*v1pb.StringifyMetadataResponse, error) {
	switch request.Engine {
	case v1pb.Engine_MYSQL, v1pb.Engine_POSTGRES, v1pb.Engine_TIDB, v1pb.Engine_ORACLE:
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported engine: %v", request.Engine)
	}

	if request.Metadata == nil {
		return nil, status.Errorf(codes.InvalidArgument, "metadata is required")
	}
	storeSchemaMetadata, _ := convertV1DatabaseMetadata(request.Metadata)
	sanitizeCommentForSchemaMetadata(storeSchemaMetadata)

	defaultSchema := extractDefaultSchemaForOracleBranch(storepb.Engine(request.Engine), storeSchemaMetadata)
	schema, err := schema.GetDesignSchema(storepb.Engine(request.Engine), defaultSchema, "" /* baseline */, storeSchemaMetadata)
	if err != nil {
		return nil, err
	}

	return &v1pb.StringifyMetadataResponse{
		Schema: schema,
	}, nil
}

func exportCSV(result *v1pb.QueryResult) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.WriteString(strings.Join(result.ColumnNames, ",")); err != nil {
		return nil, err
	}
	if err := buf.WriteByte('\n'); err != nil {
		return nil, err
	}
	for i, row := range result.Rows {
		for i, value := range row.Values {
			if i != 0 {
				if err := buf.WriteByte(','); err != nil {
					return nil, err
				}
			}
			if _, err := buf.Write(convertValueToBytesInCSV(value)); err != nil {
				return nil, err
			}
		}
		if i != len(result.Rows)-1 {
			if err := buf.WriteByte('\n'); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

func convertValueToBytesInCSV(value *v1pb.RowValue) []byte {
	switch value.Kind.(type) {
	case *v1pb.RowValue_StringValue:
		var result []byte
		result = append(result, '"')
		result = append(result, []byte(escapeCSVString(value.GetStringValue()))...)
		result = append(result, '"')
		return result
	case *v1pb.RowValue_Int32Value:
		return []byte(strconv.FormatInt(int64(value.GetInt32Value()), 10))
	case *v1pb.RowValue_Int64Value:
		return []byte(strconv.FormatInt(value.GetInt64Value(), 10))
	case *v1pb.RowValue_Uint32Value:
		return []byte(strconv.FormatUint(uint64(value.GetUint32Value()), 10))
	case *v1pb.RowValue_Uint64Value:
		return []byte(strconv.FormatUint(value.GetUint64Value(), 10))
	case *v1pb.RowValue_FloatValue:
		return []byte(strconv.FormatFloat(float64(value.GetFloatValue()), 'f', -1, 32))
	case *v1pb.RowValue_DoubleValue:
		return []byte(strconv.FormatFloat(value.GetDoubleValue(), 'f', -1, 64))
	case *v1pb.RowValue_BoolValue:
		return []byte(strconv.FormatBool(value.GetBoolValue()))
	case *v1pb.RowValue_BytesValue:
		var result []byte
		result = append(result, '"')
		result = append(result, []byte(escapeCSVString(string(value.GetBytesValue())))...)
		result = append(result, '"')
		return result
	case *v1pb.RowValue_NullValue:
		return []byte("")
	case *v1pb.RowValue_ValueValue:
		// This is used by ClickHouse and Spanner only.
		return convertValueValueToBytes(value.GetValueValue())
	default:
		return []byte("")
	}
}

func escapeCSVString(str string) string {
	escapedStr := strings.ReplaceAll(str, `"`, `""`)
	return escapedStr
}

func getSQLStatementPrefix(engine storepb.Engine, resourceList []base.SchemaResource, columnNames []string) (string, error) {
	var escapeQuote string
	switch engine {
	case storepb.Engine_MYSQL, storepb.Engine_MARIADB, storepb.Engine_TIDB, storepb.Engine_OCEANBASE, storepb.Engine_SPANNER:
		escapeQuote = "`"
	case storepb.Engine_CLICKHOUSE, storepb.Engine_MSSQL, storepb.Engine_ORACLE, storepb.Engine_OCEANBASE_ORACLE, storepb.Engine_DM, storepb.Engine_POSTGRES, storepb.Engine_REDSHIFT, storepb.Engine_SQLITE, storepb.Engine_SNOWFLAKE:
		// ClickHouse takes both double-quotes or backticks.
		escapeQuote = "\""
	default:
		// storepb.Engine_MONGODB, storepb.Engine_REDIS
		return "", errors.Errorf("unsupported engine %v for exporting as SQL", engine)
	}

	s := "INSERT INTO "
	if len(resourceList) == 1 {
		resource := resourceList[0]
		if resource.Schema != "" {
			s = fmt.Sprintf("%s%s%s%s%s", s, escapeQuote, resource.Schema, escapeQuote, ".")
		}
		s = fmt.Sprintf("%s%s%s%s", s, escapeQuote, resource.Table, escapeQuote)
	} else {
		s = fmt.Sprintf("%s%s%s%s", s, escapeQuote, "<table_name>", escapeQuote)
	}
	var columnTokens []string
	for _, columnName := range columnNames {
		columnTokens = append(columnTokens, fmt.Sprintf("%s%s%s", escapeQuote, columnName, escapeQuote))
	}
	s = fmt.Sprintf("%s (%s) VALUES (", s, strings.Join(columnTokens, ","))
	return s, nil
}

func exportSQL(engine storepb.Engine, statementPrefix string, result *v1pb.QueryResult) ([]byte, error) {
	var buf bytes.Buffer
	for i, row := range result.Rows {
		if _, err := buf.WriteString(statementPrefix); err != nil {
			return nil, err
		}
		for i, value := range row.Values {
			if i != 0 {
				if err := buf.WriteByte(','); err != nil {
					return nil, err
				}
			}
			if _, err := buf.Write(convertValueToBytesInSQL(engine, value)); err != nil {
				return nil, err
			}
		}
		if i != len(result.Rows)-1 {
			if _, err := buf.WriteString(");\n"); err != nil {
				return nil, err
			}
		} else {
			if _, err := buf.WriteString(");"); err != nil {
				return nil, err
			}
		}
	}
	return buf.Bytes(), nil
}

func convertValueToBytesInSQL(engine storepb.Engine, value *v1pb.RowValue) []byte {
	switch value.Kind.(type) {
	case *v1pb.RowValue_StringValue:
		return escapeSQLString(engine, []byte(value.GetStringValue()))
	case *v1pb.RowValue_Int32Value:
		return []byte(strconv.FormatInt(int64(value.GetInt32Value()), 10))
	case *v1pb.RowValue_Int64Value:
		return []byte(strconv.FormatInt(value.GetInt64Value(), 10))
	case *v1pb.RowValue_Uint32Value:
		return []byte(strconv.FormatUint(uint64(value.GetUint32Value()), 10))
	case *v1pb.RowValue_Uint64Value:
		return []byte(strconv.FormatUint(value.GetUint64Value(), 10))
	case *v1pb.RowValue_FloatValue:
		return []byte(strconv.FormatFloat(float64(value.GetFloatValue()), 'f', -1, 32))
	case *v1pb.RowValue_DoubleValue:
		return []byte(strconv.FormatFloat(value.GetDoubleValue(), 'f', -1, 64))
	case *v1pb.RowValue_BoolValue:
		return []byte(strconv.FormatBool(value.GetBoolValue()))
	case *v1pb.RowValue_BytesValue:
		return escapeSQLBytes(engine, value.GetBytesValue())
	case *v1pb.RowValue_NullValue:
		return []byte("NULL")
	case *v1pb.RowValue_ValueValue:
		// This is used by ClickHouse and Spanner only.
		return convertValueValueToBytes(value.GetValueValue())
	default:
		return []byte("")
	}
}

func escapeSQLString(engine storepb.Engine, v []byte) []byte {
	switch engine {
	case storepb.Engine_POSTGRES, storepb.Engine_REDSHIFT:
		escapedStr := pq.QuoteLiteral(string(v))
		return []byte(escapedStr)
	default:
		result := []byte("'")
		s := strconv.Quote(string(v))
		s = s[1 : len(s)-1]
		s = strings.ReplaceAll(s, `'`, `''`)
		result = append(result, []byte(s)...)
		result = append(result, '\'')
		return result
	}
}

func escapeSQLBytes(engine storepb.Engine, v []byte) []byte {
	switch engine {
	case storepb.Engine_MYSQL, storepb.Engine_MARIADB:
		result := []byte("B'")
		s := fmt.Sprintf("%b", v)
		s = s[1 : len(s)-1]
		result = append(result, []byte(s)...)
		result = append(result, '\'')
		return result
	default:
		return escapeSQLString(engine, v)
	}
}

func convertValueValueToBytes(value *structpb.Value) []byte {
	switch value.Kind.(type) {
	case *structpb.Value_NullValue:
		return []byte("")
	case *structpb.Value_StringValue:
		var result []byte
		result = append(result, '"')
		result = append(result, []byte(value.GetStringValue())...)
		result = append(result, '"')
		return result
	case *structpb.Value_NumberValue:
		return []byte(strconv.FormatFloat(value.GetNumberValue(), 'f', -1, 64))
	case *structpb.Value_BoolValue:
		return []byte(strconv.FormatBool(value.GetBoolValue()))
	case *structpb.Value_ListValue:
		var buf [][]byte
		for _, v := range value.GetListValue().Values {
			buf = append(buf, convertValueValueToBytes(v))
		}
		var result []byte
		result = append(result, '"')
		result = append(result, '[')
		result = append(result, bytes.Join(buf, []byte(","))...)
		result = append(result, ']')
		result = append(result, '"')
		return result
	case *structpb.Value_StructValue:
		first := true
		var buf []byte
		buf = append(buf, '"')
		for k, v := range value.GetStructValue().Fields {
			if first {
				first = false
			} else {
				buf = append(buf, ',')
			}
			buf = append(buf, []byte(k)...)
			buf = append(buf, ':')
			buf = append(buf, convertValueValueToBytes(v)...)
		}
		buf = append(buf, '"')
		return buf
	default:
		return []byte("")
	}
}

func exportJSON(result *v1pb.QueryResult) ([]byte, error) {
	var results []map[string]any
	for _, row := range result.Rows {
		m := make(map[string]any)
		for i, value := range row.Values {
			m[result.ColumnNames[i]] = convertValueToStringInJSON(value)
		}
		results = append(results, m)
	}
	return json.MarshalIndent(results, "", "  ")
}

func convertValueToStringInJSON(value *v1pb.RowValue) string {
	switch value.Kind.(type) {
	case *v1pb.RowValue_StringValue:
		return value.GetStringValue()
	case *v1pb.RowValue_Int32Value:
		return strconv.FormatInt(int64(value.GetInt32Value()), 10)
	case *v1pb.RowValue_Int64Value:
		return strconv.FormatInt(value.GetInt64Value(), 10)
	case *v1pb.RowValue_Uint32Value:
		return strconv.FormatUint(uint64(value.GetUint32Value()), 10)
	case *v1pb.RowValue_Uint64Value:
		return strconv.FormatUint(value.GetUint64Value(), 10)
	case *v1pb.RowValue_FloatValue:
		return strconv.FormatFloat(float64(value.GetFloatValue()), 'f', -1, 32)
	case *v1pb.RowValue_DoubleValue:
		return strconv.FormatFloat(value.GetDoubleValue(), 'f', -1, 64)
	case *v1pb.RowValue_BoolValue:
		return strconv.FormatBool(value.GetBoolValue())
	case *v1pb.RowValue_BytesValue:
		return base64.StdEncoding.EncodeToString(value.GetBytesValue())
	case *v1pb.RowValue_NullValue:
		return "null"
	case *v1pb.RowValue_ValueValue:
		// This is used by ClickHouse and Spanner only.
		return value.GetValueValue().String()
	default:
		return ""
	}
}

const (
	excelLetters   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	sheet1Name     = "Sheet1"
	excelMaxColumn = 18278
)

func exportXLSX(result *v1pb.QueryResult) ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()
	index, err := f.NewSheet("Sheet1")
	if err != nil {
		return nil, err
	}
	var columnPrefixes []string
	for i, columnName := range result.ColumnNames {
		columnPrefix, err := getExcelColumnName(i)
		if err != nil {
			return nil, err
		}
		columnPrefixes = append(columnPrefixes, columnPrefix)
		if err := f.SetCellValue(sheet1Name, fmt.Sprintf("%s1", columnPrefix), columnName); err != nil {
			return nil, err
		}
	}
	for i, row := range result.Rows {
		for j, value := range row.Values {
			columnName := fmt.Sprintf("%s%d", columnPrefixes[j], i+2)
			if err := f.SetCellValue("Sheet1", columnName, convertValueToStringInXLSX(value)); err != nil {
				return nil, err
			}
		}
	}
	f.SetActiveSheet(index)
	excelBytes, err := f.WriteToBuffer()
	if err != nil {
		return nil, err
	}
	return excelBytes.Bytes(), nil
}

func getExcelColumnName(index int) (string, error) {
	if index >= excelMaxColumn {
		return "", errors.Errorf("index cannot be greater than %v (column ZZZ)", excelMaxColumn)
	}

	var s string
	for {
		remain := index % 26
		s = string(excelLetters[remain]) + s
		index = index/26 - 1
		if index < 0 {
			break
		}
	}
	return s, nil
}

func convertValueToStringInXLSX(value *v1pb.RowValue) string {
	switch value.Kind.(type) {
	case *v1pb.RowValue_StringValue:
		return value.GetStringValue()
	case *v1pb.RowValue_Int32Value:
		return strconv.FormatInt(int64(value.GetInt32Value()), 10)
	case *v1pb.RowValue_Int64Value:
		return strconv.FormatInt(value.GetInt64Value(), 10)
	case *v1pb.RowValue_Uint32Value:
		return strconv.FormatUint(uint64(value.GetUint32Value()), 10)
	case *v1pb.RowValue_Uint64Value:
		return strconv.FormatUint(value.GetUint64Value(), 10)
	case *v1pb.RowValue_FloatValue:
		return strconv.FormatFloat(float64(value.GetFloatValue()), 'f', -1, 32)
	case *v1pb.RowValue_DoubleValue:
		return strconv.FormatFloat(value.GetDoubleValue(), 'f', -1, 64)
	case *v1pb.RowValue_BoolValue:
		return strconv.FormatBool(value.GetBoolValue())
	case *v1pb.RowValue_BytesValue:
		return base64.StdEncoding.EncodeToString(value.GetBytesValue())
	case *v1pb.RowValue_NullValue:
		return ""
	case *v1pb.RowValue_ValueValue:
		// This is used by ClickHouse and Spanner only.
		return value.GetValueValue().String()
	default:
		return ""
	}
}

func (s *SQLService) createExportActivity(ctx context.Context, user *store.UserMessage, level api.ActivityLevel, instanceUID int, database *store.DatabaseMessage, payload api.ActivitySQLExportPayload) (*store.ActivityMessage, error) {
	// TODO: use v1 activity API instead of
	activityBytes, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("Failed to marshal activity before exporting sql statement",
			slog.String("database_name", payload.DatabaseName),
			slog.Int("instance_id", payload.InstanceID),
			slog.String("statement", payload.Statement),
			log.BBError(err))
		return nil, status.Errorf(codes.Internal, "Failed to construct activity payload: %v", err)
	}

	activityCreate := &store.ActivityMessage{
		CreatorUID:        user.ID,
		Type:              api.ActivitySQLExport,
		ResourceContainer: fmt.Sprintf("projects/%s", database.ProjectID),
		ContainerUID:      instanceUID,
		Level:             level,
		Comment: fmt.Sprintf("Export `%q` in database %q of instance %d.",
			payload.Statement, payload.DatabaseName, payload.InstanceID),
		Payload: string(activityBytes),
	}

	activity, err := s.store.CreateActivityV2(ctx, activityCreate)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create activity: %v", err)
	}
	return activity, nil
}

// SearchQueryHistories lists query histories.
func (s *SQLService) SearchQueryHistories(ctx context.Context, request *v1pb.SearchQueryHistoriesRequest) (*v1pb.SearchQueryHistoriesResponse, error) {
	var pageToken storepb.PageToken
	if request.PageToken != "" {
		if err := unmarshalPageToken(request.PageToken, &pageToken); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid page token: %v", err)
		}
	} else {
		pageToken.Limit = request.PageSize
	}

	limit := int(pageToken.Limit)
	if limit <= 0 {
		limit = 10
	}
	if limit > 1000 {
		limit = 1000
	}
	limitPlusOne := limit + 1
	offset := int(pageToken.Offset)

	principalID, ok := ctx.Value(common.PrincipalIDContextKey).(int)
	if !ok {
		return nil, status.Errorf(codes.Internal, "principal ID not found")
	}

	find := &store.FindQueryHistoryMessage{
		CreatorUID: &principalID,
		Limit:      &limitPlusOne,
		Offset:     &offset,
	}

	filters, err := parseFilter(request.Filter)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	for _, spec := range filters {
		if spec.operator != comparatorTypeEqual {
			return nil, status.Errorf(codes.InvalidArgument, `only support "=" operation for "%v" filter`, spec.key)
		}
		switch spec.key {
		case "database":
			database := spec.value
			find.Database = &database
		case "instance":
			instance := spec.value
			find.Instance = &instance
		case "type":
			historyType := store.QueryHistoryType(spec.value)
			find.Type = &historyType
		default:
			return nil, status.Errorf(codes.InvalidArgument, "invalid filter %s", spec.key)
		}
	}

	historyList, err := s.store.ListQueryHistories(ctx, find)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list history: %v", err.Error())
	}

	nextPageToken := ""
	if len(historyList) == limitPlusOne {
		historyList = historyList[:limit]
		if nextPageToken, err = marshalPageToken(&storepb.PageToken{
			Limit:  int32(limit),
			Offset: int32(limit + offset),
		}); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to marshal next page token, error: %v", err)
		}
	}

	resp := &v1pb.SearchQueryHistoriesResponse{
		NextPageToken: nextPageToken,
	}
	for _, history := range historyList {
		queryHistory, err := s.convertToV1QueryHistory(ctx, history)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to convert log entity, error: %v", err)
		}
		if queryHistory == nil {
			continue
		}
		resp.QueryHistories = append(resp.QueryHistories, queryHistory)
	}

	return resp, nil
}

func (s *SQLService) convertToV1QueryHistory(ctx context.Context, history *store.QueryHistoryMessage) (*v1pb.QueryHistory, error) {
	user, err := s.store.GetUserByID(ctx, history.CreatorUID)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, errors.Errorf("cannot found user with id %d", history.CreatorUID)
	}

	historyType := v1pb.QueryHistory_TYPE_UNSPECIFIED
	switch history.Type {
	case store.QueryHistoryTypeExport:
		historyType = v1pb.QueryHistory_EXPORT
	case store.QueryHistoryTypeQuery:
		historyType = v1pb.QueryHistory_QUERY
	}

	return &v1pb.QueryHistory{
		Name:       fmt.Sprintf("queryHistories/%d", history.UID),
		Statement:  history.Statement,
		Error:      history.Payload.Error,
		Database:   history.Database,
		Creator:    fmt.Sprintf("%s%s", common.UserNamePrefix, user.Email),
		CreateTime: timestamppb.New(history.CreatedTime),
		Duration:   history.Payload.Duration,
		Type:       historyType,
	}, nil
}

// Query executes a SQL query.
// We have the following stages:
//  1. pre-query
//  2. do query
//  3. post-query
func (s *SQLService) Query(ctx context.Context, request *v1pb.QueryRequest) (*v1pb.QueryResponse, error) {
	// Prepare related message.
	user, environment, instance, database, err := s.prepareRelatedMessage(ctx, request.Name, request.ConnectionDatabase)
	if err != nil {
		return nil, err
	}

	statement := request.Statement
	// In Redshift datashare, Rewrite query used for parser.
	if database.DataShare {
		statement = strings.ReplaceAll(statement, fmt.Sprintf("%s.", database.DatabaseName), "")
	}

	// Validate the request.
	if err := validateQueryRequest(instance, statement); err != nil {
		return nil, err
	}

	schemaName := ""
	if instance.Engine == storepb.Engine_ORACLE {
		// For Oracle, there are two modes, schema-based and database-based management.
		// For schema-based management, also say tenant mode, we need to use the schemaName as the databaseName.
		// So the default schemaName is the database name.
		// For database-based management, we need to use the dataSource.Username as the schemaName.
		// So the default schemaName is the dataSource.Username.
		isSchemaTenantMode := (instance.Options != nil && instance.Options.GetSchemaTenantMode())
		if isSchemaTenantMode {
			schemaName = database.DatabaseName
		} else {
			dataSource, _, err := s.dbFactory.GetReadOnlyDatabaseSource(instance, database, "" /* dataSourceID */)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "failed to get read only database source: %v", err.Error())
			}
			schemaName = dataSource.Username
		}
	}

	// Get query span.
	spans, err := base.GetQuerySpan(
		ctx,
		instance.Engine,
		statement,
		database.DatabaseName,
		schemaName,
		BuildGetDatabaseMetadataFunc(s.store, instance, database.DatabaseName),
		BuildListDatabaseNamesFunc(s.store, instance),
		store.IgnoreDatabaseAndTableCaseSensitive(instance),
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get query span: %v", err.Error())
	}

	if s.licenseService.IsFeatureEnabled(api.FeatureAccessControl) == nil {
		if err := s.accessCheck(ctx, instance, user, spans, request.Limit, false /* isAdmin */, false /* isExport */); err != nil {
			return nil, err
		}
	}

	// Run SQL review.
	adviceStatus, advices, err := s.sqlReviewCheck(ctx, statement, v1pb.CheckRequest_CHANGE_TYPE_UNSPECIFIED, environment, instance, database, nil /* Override Metadata */)
	if err != nil {
		return nil, err
	}
	// Create query activity.
	level := api.ActivityInfo
	switch adviceStatus {
	case advisor.Error:
		level = api.ActivityError
	case advisor.Warn:
		level = api.ActivityWarn
	}

	activity, err := s.createQueryActivity(ctx, user, level, instance.UID, database, api.ActivitySQLEditorQueryPayload{
		Statement:    request.Statement,
		InstanceID:   instance.UID,
		DatabaseID:   database.UID,
		DatabaseName: database.DatabaseName,
	})
	if err != nil {
		return nil, err
	}

	var results []*v1pb.QueryResult
	var queryErr error
	var durationNs int64
	if adviceStatus != advisor.Error {
		results, durationNs, queryErr = s.doQuery(ctx, request, instance, database)
		if queryErr == nil && s.licenseService.IsFeatureEnabledForInstance(api.FeatureSensitiveData, instance) == nil {
			masker := NewQueryResultMasker(s.store)
			if err := masker.MaskResults(ctx, spans, results, instance, storepb.MaskingExceptionPolicy_MaskingException_QUERY); err != nil {
				return nil, status.Errorf(codes.Internal, err.Error())
			}
		}
	}

	// Update activity.
	if err = s.postQuery(ctx, database, activity, durationNs, queryErr); err != nil {
		return nil, err
	}
	if queryErr != nil {
		return nil, status.Errorf(codes.Internal, queryErr.Error())
	}

	allowExport := true
	// AllowExport is a validate only check.
	if s.licenseService.IsFeatureEnabled(api.FeatureAccessControl) == nil {
		err := s.accessCheck(ctx, instance, user, spans, request.Limit, false /* isAdmin */, true /* isExport */)
		allowExport = (err == nil)
	}

	response := &v1pb.QueryResponse{
		Results:     results,
		Advices:     advices,
		AllowExport: allowExport,
	}

	if proto.Size(response) > maximumSQLResultSize {
		response.Results = []*v1pb.QueryResult{
			{
				Error: fmt.Sprintf("Output of query exceeds max allowed output size of %dMB", maximumSQLResultSize/1024/1024),
			},
		}
	}

	return response, nil
}

// doQuery does query.
func (s *SQLService) doQuery(ctx context.Context, request *v1pb.QueryRequest, instance *store.InstanceMessage, database *store.DatabaseMessage) ([]*v1pb.QueryResult, int64, error) {
	driver, err := s.dbFactory.GetReadOnlyDatabaseDriver(ctx, instance, database, request.DataSourceId)
	if err != nil {
		return nil, 0, err
	}
	defer driver.Close(ctx)

	sqlDB := driver.GetDB()
	var conn *sql.Conn
	if sqlDB != nil {
		conn, err = sqlDB.Conn(ctx)
		if err != nil {
			return nil, 0, err
		}
		defer conn.Close()
	}

	timeout := defaultTimeout
	if request.Timeout != nil {
		timeout = request.Timeout.AsDuration()
	}
	ctx, cancelCtx := context.WithTimeout(ctx, timeout)
	defer cancelCtx()

	start := time.Now().UnixNano()
	results, err := driver.QueryConn(ctx, conn, request.Statement, &db.QueryContext{
		Limit:               int(request.Limit),
		ReadOnly:            true,
		CurrentDatabase:     database.DatabaseName,
		SensitiveSchemaInfo: nil,
		EnableSensitive:     s.licenseService.IsFeatureEnabledForInstance(api.FeatureSensitiveData, instance) == nil,
	})
	select {
	case <-ctx.Done():
		// canceled or timed out
		return nil, time.Now().UnixNano() - start, errors.Errorf("timeout reached: %v", timeout)
	default:
		// So the select will not block
	}

	sanitizeResults(results)

	return results, time.Now().UnixNano() - start, err
}

// postQuery does the following:
//  1. Check index hit Explain statements
//  2. Update SQL query activity
func (s *SQLService) postQuery(ctx context.Context, database *store.DatabaseMessage, activity *store.ActivityMessage, durationNs int64, queryErr error) error {
	newLevel := activity.Level

	// Update the activity
	var payload api.ActivitySQLEditorQueryPayload
	if err := json.Unmarshal([]byte(activity.Payload), &payload); err != nil {
		return status.Errorf(codes.Internal, "failed to unmarshal activity payload: %v", err)
	}

	payload.DurationNs = durationNs
	if queryErr != nil {
		payload.Error = queryErr.Error()
		newLevel = api.ActivityError
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("Failed to marshal activity after executing sql statement",
			slog.String("database_name", payload.DatabaseName),
			slog.Int("instance_id", payload.InstanceID),
			slog.String("statement", payload.Statement),
			log.BBError(err))
		return status.Errorf(codes.Internal, "Failed to marshal activity after executing sql statement: %v", err)
	}

	payloadString := string(payloadBytes)
	if _, err := s.store.UpdateActivityV2(ctx, &store.UpdateActivityMessage{
		UID:     activity.UID,
		Level:   &newLevel,
		Payload: &payloadString,
	}); err != nil {
		return status.Errorf(codes.Internal, "Failed to update activity after executing sql statement: %v", err)
	}

	if _, err := s.store.CreateQueryHistory(ctx, &store.QueryHistoryMessage{
		CreatorUID: activity.CreatorUID,
		ProjectID:  database.ProjectID,
		Database:   common.FormatDatabase(database.InstanceID, database.DatabaseName),
		Statement:  payload.Statement,
		Type:       store.QueryHistoryTypeQuery,
		Payload: &storepb.QueryHistoryPayload{
			Error:    &payload.Error,
			Duration: durationpb.New(time.Duration(durationNs)),
		},
	}); err != nil {
		return status.Errorf(codes.Internal, "Failed to create export history with error: %v", err)
	}

	return nil
}

func BuildGetDatabaseMetadataFunc(storeInstance *store.Store, instance *store.InstanceMessage, connectionDatabase string) base.GetDatabaseMetadataFunc {
	if instance.Engine == storepb.Engine_ORACLE {
		return func(ctx context.Context, schemaName string) (string, *model.DatabaseMetadata, error) {
			// There are two modes for Oracle, schema-based and database-based management.
			// For schema-based management, also say tenant mode, we need to use the schemaName as the databaseName.
			// For database-based management, we need to use the connectionDatabase as the databaseName.
			databaseName := connectionDatabase
			isSchemaTenantMode := (instance.Options != nil && instance.Options.GetSchemaTenantMode())
			if isSchemaTenantMode {
				databaseName = schemaName
			}

			database, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
				InstanceID:   &instance.ResourceID,
				DatabaseName: &databaseName,
			})
			if err != nil {
				return "", nil, err
			}
			if database == nil {
				return "", nil, nil
			}
			databaseMetadata, err := storeInstance.GetDBSchema(ctx, database.UID)
			if err != nil {
				return "", nil, err
			}
			if databaseMetadata == nil {
				return "", nil, nil
			}
			return databaseName, databaseMetadata.GetDatabaseMetadata(), nil
		}
	}
	return func(ctx context.Context, databaseName string) (string, *model.DatabaseMetadata, error) {
		database, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
			InstanceID:   &instance.ResourceID,
			DatabaseName: &databaseName,
		})
		if err != nil {
			return "", nil, err
		}
		if database == nil {
			return "", nil, nil
		}
		databaseMetadata, err := storeInstance.GetDBSchema(ctx, database.UID)
		if err != nil {
			return "", nil, err
		}
		if databaseMetadata == nil {
			return "", nil, nil
		}
		return databaseName, databaseMetadata.GetDatabaseMetadata(), nil
	}
}

func BuildListDatabaseNamesFunc(storeInstance *store.Store, instance *store.InstanceMessage) base.ListDatabaseNamesFunc {
	return func(ctx context.Context) ([]string, error) {
		databases, err := storeInstance.ListDatabases(ctx, &store.FindDatabaseMessage{
			InstanceID: &instance.ResourceID,
		})
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(databases))
		for _, database := range databases {
			names = append(names, database.DatabaseName)
		}
		return names, nil
	}
}

func (s *SQLService) accessCheck(
	ctx context.Context,
	instance *store.InstanceMessage,
	user *store.UserMessage,
	spans []*base.QuerySpan,
	limit int32,
	isAdmin,
	isExport bool) error {
	// Check if the caller is admin for exporting with admin mode.
	if isAdmin && isExport && (user.Role != api.WorkspaceAdmin && user.Role != api.WorkspaceDBA) {
		return status.Errorf(codes.PermissionDenied, "only workspace owner and DBA can export data using admin mode")
	}

	for _, span := range spans {
		for column := range span.SourceColumns {
			databaseResourceURL := common.FormatDatabase(instance.ResourceID, column.Database)
			attributes := map[string]any{
				"request.time":      time.Now(),
				"resource.database": databaseResourceURL,
				"resource.schema":   column.Schema,
				"resource.table":    column.Table,
				"request.row_limit": limit,
			}

			project, database, err := s.getProjectAndDatabaseMessage(ctx, instance, column.Database)
			if err != nil {
				return status.Errorf(codes.Internal, err.Error())
			}
			if project == nil && database == nil {
				// If database not found, skip.
				// TODO(d): re-evaluate this case.
				continue
			}
			if project == nil {
				// Never happen
				return status.Errorf(codes.Internal, "project not found for database: %s", column.Database)
			}
			// Allow query databases across different projects.
			projectPolicy, err := s.store.GetProjectPolicy(ctx, &store.GetProjectPolicyMessage{ProjectID: &project.ResourceID})
			if err != nil {
				return status.Errorf(codes.Internal, err.Error())
			}

			ok, err := s.hasDatabaseAccessRights(ctx, user, projectPolicy, attributes, isExport)
			if err != nil {
				return status.Errorf(codes.Internal, "failed to check access control for database: %q, error %v", column.Database, err)
			}
			if !ok {
				return status.Errorf(codes.PermissionDenied, "permission denied to access resource: %q", column.String())
			}
		}
	}

	return nil
}

func mask(maskers []masker.Masker, result *v1pb.QueryResult) {
	sensitive := make([]bool, len(result.ColumnNames))
	for i := range result.ColumnNames {
		if i < len(maskers) {
			switch maskers[i].(type) {
			case *masker.NoneMasker:
				sensitive[i] = false
			default:
				sensitive[i] = true
			}
		}
	}

	for i, row := range result.Rows {
		for j, value := range row.Values {
			if value == nil {
				continue
			}
			maskedValue := row.Values[j]
			if j < len(maskers) && maskers[j] != nil {
				maskedValue = maskers[j].Mask(&masker.MaskData{
					DataV2: row.Values[j],
				})
			}
			result.Rows[i].Values[j] = maskedValue
		}
	}

	result.Sensitive = sensitive
	result.Masked = sensitive
}

// sanitizeResults sanitizes the strings in the results by replacing all the invalid UTF-8 characters with its hexadecimal representation.
func sanitizeResults(results []*v1pb.QueryResult) {
	for _, result := range results {
		for _, row := range result.GetRows() {
			for _, value := range row.GetValues() {
				if value, ok := value.Kind.(*v1pb.RowValue_StringValue); ok {
					value.StringValue = common.SanitizeUTF8String(value.StringValue)
				}
			}
		}
	}
}

func (s *SQLService) createQueryActivity(ctx context.Context, user *store.UserMessage, level api.ActivityLevel, instanceUID int, database *store.DatabaseMessage, payload api.ActivitySQLEditorQueryPayload) (*store.ActivityMessage, error) {
	// TODO: use v1 activity API instead of
	activityBytes, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("Failed to marshal activity before executing sql statement",
			slog.String("database_name", payload.DatabaseName),
			slog.Int("instance_id", payload.InstanceID),
			slog.String("statement", payload.Statement),
			log.BBError(err))
		return nil, status.Errorf(codes.Internal, "Failed to construct activity payload: %v", err)
	}

	activityCreate := &store.ActivityMessage{
		CreatorUID:        user.ID,
		Type:              api.ActivitySQLQuery,
		ResourceContainer: fmt.Sprintf("projects/%s", database.ProjectID),
		ContainerUID:      instanceUID,
		Level:             level,
		Comment: fmt.Sprintf("Executed `%q` in database %q of instance %d.",
			payload.Statement, payload.DatabaseName, payload.InstanceID),
		Payload: string(activityBytes),
	}

	activity, err := s.store.CreateActivityV2(ctx, activityCreate)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Failed to create activity: %v", err)
	}

	return activity, nil
}

func getMaskerByMaskingAlgorithmAndLevel(algorithm *storepb.MaskingAlgorithmSetting_Algorithm, level storepb.MaskingLevel) masker.Masker {
	if algorithm == nil {
		switch level {
		case storepb.MaskingLevel_FULL:
			return masker.NewDefaultFullMasker()
		case storepb.MaskingLevel_PARTIAL:
			return masker.NewDefaultRangeMasker()
		default:
			return masker.NewNoneMasker()
		}
	}

	switch m := algorithm.Mask.(type) {
	case *storepb.MaskingAlgorithmSetting_Algorithm_FullMask_:
		return masker.NewFullMasker(m.FullMask.Substitution)
	case *storepb.MaskingAlgorithmSetting_Algorithm_RangeMask_:
		return masker.NewRangeMasker(convertRangeMaskSlices(m.RangeMask.Slices))
	case *storepb.MaskingAlgorithmSetting_Algorithm_Md5Mask:
		return masker.NewMD5Masker(m.Md5Mask.Salt)
	}
	return masker.NewNoneMasker()
}

func convertRangeMaskSlices(slices []*storepb.MaskingAlgorithmSetting_Algorithm_RangeMask_Slice) []*masker.MaskRangeSlice {
	var result []*masker.MaskRangeSlice
	for _, slice := range slices {
		result = append(result, &masker.MaskRangeSlice{
			Start:        slice.Start,
			End:          slice.End,
			Substitution: slice.Substitution,
		})
	}
	return result
}

// getReadOnlyDataSource returns the read-only data source for the instance.
// If the read-only data source is not defined, we will fallback to admin data source.
func getReadOnlyDataSource(instance *store.InstanceMessage) *store.DataSourceMessage {
	dataSource := utils.DataSourceFromInstanceWithType(instance, api.RO)
	adminDataSource := utils.DataSourceFromInstanceWithType(instance, api.Admin)
	if dataSource == nil {
		dataSource = adminDataSource
	}
	return dataSource
}

// sqlReviewCheck checks the SQL statement against the SQL review policy bind to given environment,
// against the database schema bind to the given database, if the overrideMetadata is provided,
// it will be used instead of fetching the database schema from the store.
func (s *SQLService) sqlReviewCheck(ctx context.Context, statement string, changeType v1pb.CheckRequest_ChangeType, environment *store.EnvironmentMessage, instance *store.InstanceMessage, database *store.DatabaseMessage, overrideMetadata *storepb.DatabaseSchemaMetadata) (advisor.Status, []*v1pb.Advice, error) {
	if !IsSQLReviewSupported(instance.Engine) || database == nil {
		return advisor.Success, nil, nil
	}

	dbMetadata := overrideMetadata
	if dbMetadata == nil {
		dbSchema, err := s.store.GetDBSchema(ctx, database.UID)
		if err != nil {
			return advisor.Error, nil, status.Errorf(codes.Internal, "failed to fetch database schema: %v", err)
		}
		if dbSchema == nil {
			if err := s.schemaSyncer.SyncDatabaseSchema(ctx, database, true /* force */); err != nil {
				return advisor.Error, nil, status.Errorf(codes.Internal, "failed to sync database schema: %v", err)
			}
			dbSchema, err = s.store.GetDBSchema(ctx, database.UID)
			if err != nil {
				return advisor.Error, nil, status.Errorf(codes.Internal, "failed to fetch database schema: %v", err)
			}
			if dbSchema == nil {
				return advisor.Error, nil, status.Errorf(codes.NotFound, "database schema not found: %v", database.UID)
			}
		}
		dbMetadata = dbSchema.GetMetadata()
	}

	catalog, err := s.store.NewCatalog(ctx, database.UID, instance.Engine, store.IgnoreDatabaseAndTableCaseSensitive(instance), overrideMetadata, advisor.SyntaxModeNormal)
	if err != nil {
		return advisor.Error, nil, status.Errorf(codes.Internal, "Failed to create a catalog: %v", err)
	}

	currentSchema := ""
	if instance.Engine == storepb.Engine_ORACLE || instance.Engine == storepb.Engine_DM || instance.Engine == storepb.Engine_OCEANBASE_ORACLE {
		if instance.Options == nil || !instance.Options.SchemaTenantMode {
			currentSchema = getReadOnlyDataSource(instance).Username
		} else {
			currentSchema = database.DatabaseName
		}
	}

	driver, err := s.dbFactory.GetAdminDatabaseDriver(ctx, instance, database, db.ConnectionContext{UseDatabaseOwner: true})
	if err != nil {
		return advisor.Error, nil, status.Errorf(codes.Internal, "Failed to get database driver: %v", err)
	}
	defer driver.Close(ctx)
	connection := driver.GetDB()
	adviceLevel, adviceList, err := s.sqlCheck(
		ctx,
		instance.Engine,
		dbMetadata,
		environment.UID,
		statement,
		changeType,
		catalog,
		connection,
		currentSchema,
		database.DatabaseName,
	)
	if err != nil {
		return advisor.Error, nil, status.Errorf(codes.Internal, "Failed to check SQL review policy: %v", err)
	}

	return adviceLevel, convertAdviceList(adviceList), nil
}

func convertAdviceList(list []advisor.Advice) []*v1pb.Advice {
	var result []*v1pb.Advice
	for _, advice := range list {
		result = append(result, &v1pb.Advice{
			Status:  convertAdviceStatus(advice.Status),
			Code:    int32(advice.Code),
			Title:   advice.Title,
			Content: advice.Content,
			Line:    int32(advice.Line),
			Column:  int32(advice.Column),
			Detail:  advice.Details,
		})
	}
	return result
}

func convertAdviceStatus(status advisor.Status) v1pb.Advice_Status {
	switch status {
	case advisor.Success:
		return v1pb.Advice_SUCCESS
	case advisor.Warn:
		return v1pb.Advice_WARNING
	case advisor.Error:
		return v1pb.Advice_ERROR
	default:
		return v1pb.Advice_STATUS_UNSPECIFIED
	}
}

func (s *SQLService) sqlCheck(
	ctx context.Context,
	dbType storepb.Engine,
	dbSchema *storepb.DatabaseSchemaMetadata,
	environmentID int,
	statement string,
	changeType v1pb.CheckRequest_ChangeType,
	catalog catalog.Catalog,
	driver *sql.DB,
	currentSchema string,
	currentDatabase string,
) (advisor.Status, []advisor.Advice, error) {
	var adviceList []advisor.Advice
	policy, err := s.store.GetSQLReviewPolicy(ctx, environmentID)
	if err != nil {
		if e, ok := err.(*common.Error); ok && e.Code == common.NotFound {
			return advisor.Success, nil, nil
		}
		return advisor.Error, nil, err
	}

	res, err := advisor.SQLReviewCheck(statement, policy.RuleList, advisor.SQLReviewCheckContext{
		Charset:         dbSchema.CharacterSet,
		Collation:       dbSchema.Collation,
		ChangeType:      convertChangeType(changeType),
		DBSchema:        dbSchema,
		DbType:          dbType,
		Catalog:         catalog,
		Driver:          driver,
		Context:         ctx,
		CurrentSchema:   currentSchema,
		CurrentDatabase: currentDatabase,
	})
	if err != nil {
		return advisor.Error, nil, err
	}

	adviceLevel := advisor.Success
	for _, advice := range res {
		switch advice.Status {
		case advisor.Warn:
			if adviceLevel != advisor.Error {
				adviceLevel = advisor.Warn
			}
		case advisor.Error:
			adviceLevel = advisor.Error
		case advisor.Success:
			continue
		}

		adviceList = append(adviceList, advice)
	}

	return adviceLevel, adviceList, nil
}

func (s *SQLService) prepareRelatedMessage(ctx context.Context, requestName string, requestDatabaseName string) (*store.UserMessage, *store.EnvironmentMessage, *store.InstanceMessage, *store.DatabaseMessage, error) {
	user, err := s.getUser(ctx)
	if err != nil {
		return nil, nil, nil, nil, status.Errorf(codes.Internal, err.Error())
	}

	var instanceID, databaseName string
	if strings.Contains(requestName, "/databases/") {
		instanceID, databaseName, err = common.GetInstanceDatabaseID(requestName)
		if err != nil {
			return nil, nil, nil, nil, status.Error(codes.InvalidArgument, err.Error())
		}
	} else {
		instanceID, err = common.GetInstanceID(requestName)
		if err != nil {
			return nil, nil, nil, nil, status.Error(codes.InvalidArgument, err.Error())
		}
		databaseName = requestDatabaseName
	}

	find := &store.FindInstanceMessage{}
	instanceUID, isNumber := isNumber(instanceID)
	if isNumber {
		find.UID = &instanceUID
	} else {
		find.ResourceID = &instanceID
	}

	instance, err := s.store.GetInstanceV2(ctx, find)
	if err != nil {
		return nil, nil, nil, nil, status.Errorf(codes.Internal, err.Error())
	}
	if instance == nil {
		return nil, nil, nil, nil, status.Errorf(codes.NotFound, "instance %q not found", instanceID)
	}

	database, err := s.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
		InstanceID:          &instance.ResourceID,
		DatabaseName:        &databaseName,
		IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
	})
	if err != nil {
		return nil, nil, nil, nil, status.Errorf(codes.Internal, "failed to fetch database: %v", err)
	}
	if database == nil {
		return nil, nil, nil, nil, status.Errorf(codes.NotFound, "database %q not found", databaseName)
	}

	environment, err := s.store.GetEnvironmentV2(ctx, &store.FindEnvironmentMessage{ResourceID: &database.EffectiveEnvironmentID})
	if err != nil {
		return nil, nil, nil, nil, status.Errorf(codes.Internal, "failed to fetch environment: %v", err)
	}
	if environment == nil {
		return nil, nil, nil, nil, status.Errorf(codes.NotFound, "environment ID not found: %s", database.EffectiveEnvironmentID)
	}

	return user, environment, instance, database, nil
}

func validateQueryRequest(instance *store.InstanceMessage, statement string) error {
	ok, err := base.ValidateSQLForEditor(instance.Engine, statement)
	if err != nil {
		syntaxErr, ok := err.(*base.SyntaxError)
		if ok {
			querySyntaxError, err := status.New(codes.InvalidArgument, err.Error()).WithDetails(
				&v1pb.PlanCheckRun_Result_SqlReviewReport{
					Line:   int32(syntaxErr.Line),
					Column: int32(syntaxErr.Column),
					Detail: syntaxErr.Message,
				},
			)
			if err != nil {
				return syntaxErr
			}
			return querySyntaxError.Err()
		}
		return err
	}
	if !ok {
		return nonSelectSQLError.Err()
	}
	return nil
}

func extractResourceList(ctx context.Context, storeInstance *store.Store, engine storepb.Engine, databaseName string, statement string, instance *store.InstanceMessage) ([]base.SchemaResource, error) {
	switch engine {
	case storepb.Engine_MYSQL, storepb.Engine_MARIADB, storepb.Engine_OCEANBASE:
		list, err := base.ExtractResourceList(engine, databaseName, "", statement)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to extract resource list: %s", err.Error())
		} else if databaseName == "" {
			return list, nil
		}

		databaseMessage, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
			InstanceID:          &instance.ResourceID,
			DatabaseName:        &databaseName,
			IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
		})
		if err != nil {
			if httpErr, ok := err.(*echo.HTTPError); ok && httpErr.Code == echo.ErrNotFound.Code {
				// If database not found, skip.
				return nil, nil
			}
			return nil, status.Errorf(codes.Internal, "failed to fetch database: %v", err)
		}
		if databaseMessage == nil {
			return nil, nil
		}

		dbSchema, err := storeInstance.GetDBSchema(ctx, databaseMessage.UID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch database schema: %v", err)
		}

		var result []base.SchemaResource
		for _, resource := range list {
			if resource.Database != dbSchema.GetMetadata().Name {
				// MySQL allows cross-database query, we should check the corresponding database.
				resourceDB, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
					InstanceID:          &instance.ResourceID,
					DatabaseName:        &resource.Database,
					IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
				})
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if resourceDB == nil {
					continue
				}
				resourceDBSchema, err := storeInstance.GetDBSchema(ctx, resourceDB.UID)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database schema %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if !resourceDBSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
					!resourceDBSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
					// If table not found, we regard it as a CTE/alias/... and skip.
					continue
				}
				result = append(result, resource)
				continue
			}
			if !dbSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
				!dbSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
				// If table not found, skip.
				continue
			}
			result = append(result, resource)
		}
		return result, nil
	case storepb.Engine_POSTGRES, storepb.Engine_REDSHIFT:
		list, err := base.ExtractResourceList(engine, databaseName, "public", statement)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to extract resource list: %s", err.Error())
		}

		databaseMessage, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
			InstanceID:          &instance.ResourceID,
			DatabaseName:        &databaseName,
			IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch database: %v", err)
		}
		if databaseMessage == nil {
			return nil, nil
		}

		dbSchema, err := storeInstance.GetDBSchema(ctx, databaseMessage.UID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch database schema: %v", err)
		}

		var result []base.SchemaResource
		for _, resource := range list {
			if resource.Database != dbSchema.GetMetadata().Name {
				// Should not happen.
				continue
			}

			if !dbSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
				!dbSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
				// If table not found, skip.
				continue
			}

			result = append(result, resource)
		}

		return result, nil
	case storepb.Engine_ORACLE, storepb.Engine_DM, storepb.Engine_OCEANBASE_ORACLE:
		dataSource := utils.DataSourceFromInstanceWithType(instance, api.RO)
		adminDataSource := utils.DataSourceFromInstanceWithType(instance, api.Admin)
		// If there are no read-only data source, fall back to admin data source.
		if dataSource == nil {
			dataSource = adminDataSource
		}
		if dataSource == nil {
			return nil, status.Errorf(codes.Internal, "failed to find data source for instance: %s", instance.ResourceID)
		}
		currentSchema := dataSource.Username
		if instance.Options != nil && instance.Options.SchemaTenantMode {
			currentSchema = databaseName
		}
		list, err := base.ExtractResourceList(engine, databaseName, currentSchema, statement)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to extract resource list: %s", err.Error())
		}

		databaseMessage, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
			InstanceID:          &instance.ResourceID,
			DatabaseName:        &databaseName,
			IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch database: %v", err)
		}
		if databaseMessage == nil {
			return nil, nil
		}

		dbSchema, err := storeInstance.GetDBSchema(ctx, databaseMessage.UID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch database schema: %v", err)
		}

		var result []base.SchemaResource
		for _, resource := range list {
			if resource.Database != dbSchema.GetMetadata().Name {
				if instance.Options == nil || !instance.Options.SchemaTenantMode {
					continue
				}
				// Schema tenant mode allows cross-database query, we should check the corresponding database.
				resourceDB, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
					InstanceID:          &instance.ResourceID,
					DatabaseName:        &resource.Database,
					IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
				})
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if resourceDB == nil {
					continue
				}
				resourceDBSchema, err := storeInstance.GetDBSchema(ctx, resourceDB.UID)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database schema %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if !resourceDBSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
					!resourceDBSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
					// If table not found, we regard it as a CTE/alias/... and skip.
					continue
				}
				result = append(result, resource)
				continue
			}

			if !dbSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
				!dbSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
				// If table not found, skip.
				continue
			}

			result = append(result, resource)
		}

		return result, nil
	case storepb.Engine_SNOWFLAKE:
		dataSource := utils.DataSourceFromInstanceWithType(instance, api.RO)
		adminDataSource := utils.DataSourceFromInstanceWithType(instance, api.Admin)
		// If there are no read-only data source, fall back to admin data source.
		if dataSource == nil {
			dataSource = adminDataSource
		}
		if dataSource == nil {
			return nil, status.Errorf(codes.Internal, "failed to find data source for instance: %s", instance.ResourceID)
		}
		list, err := base.ExtractResourceList(engine, databaseName, "PUBLIC", statement)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to extract resource list: %s", err.Error())
		}
		databaseMessage, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
			InstanceID:          &instance.ResourceID,
			DatabaseName:        &databaseName,
			IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
		})
		if err != nil {
			if httpErr, ok := err.(*echo.HTTPError); ok && httpErr.Code == echo.ErrNotFound.Code {
				// If database not found, skip.
				return nil, nil
			}
			return nil, status.Errorf(codes.Internal, "failed to fetch database: %v", err)
		}
		if databaseMessage == nil {
			return nil, nil
		}

		dbSchema, err := storeInstance.GetDBSchema(ctx, databaseMessage.UID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch database schema: %v", err)
		}

		var result []base.SchemaResource
		for _, resource := range list {
			if resource.Database != dbSchema.GetMetadata().Name {
				// Snowflake allows cross-database query, we should check the corresponding database.
				resourceDB, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
					InstanceID:          &instance.ResourceID,
					DatabaseName:        &resource.Database,
					IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
				})
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if resourceDB == nil {
					continue
				}
				resourceDBSchema, err := storeInstance.GetDBSchema(ctx, resourceDB.UID)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database schema %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if !resourceDBSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
					!resourceDBSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
					// If table not found, we regard it as a CTE/alias/... and skip.
					continue
				}
				result = append(result, resource)
				continue
			}
			if !dbSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
				!dbSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
				// If table not found, skip.
				continue
			}
			result = append(result, resource)
		}
		return result, nil
	case storepb.Engine_MSSQL:
		dataSource := utils.DataSourceFromInstanceWithType(instance, api.RO)
		adminDataSource := utils.DataSourceFromInstanceWithType(instance, api.Admin)
		// If there are no read-only data source, fall back to admin data source.
		if dataSource == nil {
			dataSource = adminDataSource
		}
		if dataSource == nil {
			return nil, status.Errorf(codes.Internal, "failed to find data source for instance: %s", instance.ResourceID)
		}
		list, err := base.ExtractResourceList(engine, databaseName, "dbo", statement)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to extract resource list: %s", err.Error())
		}
		databaseMessage, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
			InstanceID:          &instance.ResourceID,
			DatabaseName:        &databaseName,
			IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
		})
		if err != nil {
			if httpErr, ok := err.(*echo.HTTPError); ok && httpErr.Code == echo.ErrNotFound.Code {
				// If database not found, skip.
				return nil, nil
			}
			return nil, status.Errorf(codes.Internal, "failed to fetch database: %v", err)
		}
		if databaseMessage == nil {
			return nil, nil
		}

		dbSchema, err := storeInstance.GetDBSchema(ctx, databaseMessage.UID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to fetch database schema: %v", err)
		}

		var result []base.SchemaResource
		for _, resource := range list {
			if resource.LinkedServer != "" {
				continue
			}
			if resource.Database != dbSchema.GetMetadata().Name {
				// MSSQL allows cross-database query, we should check the corresponding database.
				resourceDB, err := storeInstance.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
					InstanceID:          &instance.ResourceID,
					DatabaseName:        &resource.Database,
					IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
				})
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if resourceDB == nil {
					continue
				}
				resourceDBSchema, err := storeInstance.GetDBSchema(ctx, resourceDB.UID)
				if err != nil {
					return nil, status.Errorf(codes.Internal, "failed to get database schema %v in instance %v, err: %v", resource.Database, instance.ResourceID, err)
				}
				if !resourceDBSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
					!resourceDBSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
					// If table not found, we regard it as a CTE/alias/... and skip.
					continue
				}
				result = append(result, resource)
				continue
			}
			if !dbSchema.TableExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) &&
				!dbSchema.ViewExists(resource.Schema, resource.Table, store.IgnoreDatabaseAndTableCaseSensitive(instance)) {
				// If table not found, skip.
				continue
			}
			result = append(result, resource)
		}
		return result, nil
	default:
		return base.ExtractResourceList(engine, databaseName, "", statement)
	}
}

func (s *SQLService) hasDatabaseAccessRights(ctx context.Context, user *store.UserMessage, projectPolicy *store.IAMPolicyMessage, attributes map[string]any, isExport bool) (bool, error) {
	wantPermission := iam.PermissionDatabasesQuery
	if isExport {
		wantPermission = iam.PermissionDatabasesExport
	}

	for _, role := range user.Roles {
		permissions, err := s.iamManager.GetPermissions(ctx, common.FormatRole(role.String()))
		if err != nil {
			return false, errors.Wrapf(err, "failed to get permissions")
		}
		if slices.Contains(permissions, wantPermission) {
			return true, nil
		}
	}

	for _, binding := range projectPolicy.Bindings {
		role := common.FormatRole(binding.Role.String())
		permissions, err := s.iamManager.GetPermissions(ctx, role)
		if err != nil {
			return false, errors.Wrapf(err, "failed to get permissions")
		}
		if !slices.Contains(permissions, wantPermission) {
			continue
		}
		hasUser := false
		for _, member := range binding.Members {
			if member.ID == user.ID || member.Email == api.AllUsers {
				hasUser = true
				break
			}
		}
		if !hasUser {
			continue
		}
		ok, err := evaluateQueryExportPolicyCondition(binding.Condition.GetExpression(), attributes)
		if err != nil {
			slog.Error("failed to evaluate condition", log.BBError(err), slog.String("condition", binding.Condition.GetExpression()))
			continue
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func evaluateMaskingExceptionPolicyCondition(expression string, attributes map[string]any) (bool, error) {
	if expression == "" {
		return true, nil
	}
	maskingExceptionPolicyEnv, err := cel.NewEnv(
		cel.Variable("resource", cel.MapType(cel.StringType, cel.AnyType)),
		cel.Variable("request", cel.MapType(cel.StringType, cel.AnyType)),
	)
	if err != nil {
		return false, errors.Wrapf(err, "failed to create CEL environment for masking exception policy")
	}
	ast, issues := maskingExceptionPolicyEnv.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return false, errors.Wrapf(issues.Err(), "failed to get the ast of CEL program for masking exception policy")
	}
	prg, err := maskingExceptionPolicyEnv.Program(ast)
	if err != nil {
		return false, errors.Wrapf(err, "failed to create CEL program for masking exception policy")
	}
	out, _, err := prg.Eval(attributes)
	if err != nil {
		return false, errors.Wrapf(err, "failed to eval CEL program for masking exception policy")
	}
	val, err := out.ConvertToNative(reflect.TypeOf(false))
	if err != nil {
		return false, errors.Wrap(err, "expect bool result for masking exception policy")
	}
	boolVar, ok := val.(bool)
	if !ok {
		return false, errors.Wrap(err, "expect bool result for masking exception policy")
	}
	return boolVar, nil
}

func evaluateMaskingRulePolicyCondition(expression string, attributes map[string]any) (bool, error) {
	if expression == "" {
		return true, nil
	}
	maskingRulePolicyEnv, err := cel.NewEnv(common.MaskingRulePolicyCELAttributes...)
	if err != nil {
		return false, errors.Wrapf(err, "failed to create CEL environment for masking rule policy")
	}
	ast, issues := maskingRulePolicyEnv.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return false, errors.Wrapf(issues.Err(), "failed to get the ast of CEL program for masking rule")
	}
	prg, err := maskingRulePolicyEnv.Program(ast)
	if err != nil {
		return false, errors.Wrapf(err, "failed to create CEL program for masking rule")
	}
	out, _, err := prg.Eval(attributes)
	if err != nil {
		return false, errors.Wrapf(err, "failed to eval CEL program for masking rule")
	}
	val, err := out.ConvertToNative(reflect.TypeOf(false))
	if err != nil {
		return false, errors.Wrap(err, "expect bool result for masking rule")
	}
	boolVar, ok := val.(bool)
	if !ok {
		return false, errors.Wrap(err, "expect bool result for masking rule")
	}
	return boolVar, nil
}

func evaluateQueryExportPolicyCondition(expression string, attributes map[string]any) (bool, error) {
	if expression == "" {
		return true, nil
	}
	env, err := cel.NewEnv(common.IAMPolicyConditionCELAttributes...)
	if err != nil {
		return false, err
	}
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return false, issues.Err()
	}
	prg, err := env.Program(ast)
	if err != nil {
		return false, err
	}

	out, _, err := prg.Eval(attributes)
	if err != nil {
		return false, err
	}
	val, err := out.ConvertToNative(reflect.TypeOf(false))
	if err != nil {
		return false, errors.Wrap(err, "expect bool result")
	}
	boolVal, ok := val.(bool)
	if !ok {
		return false, errors.Wrap(err, "failed to convert to bool")
	}
	return boolVal, nil
}

func (s *SQLService) getProjectAndDatabaseMessage(ctx context.Context, instance *store.InstanceMessage, database string) (*store.ProjectMessage, *store.DatabaseMessage, error) {
	databaseMessage, err := s.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
		InstanceID:          &instance.ResourceID,
		DatabaseName:        &database,
		IgnoreCaseSensitive: store.IgnoreDatabaseAndTableCaseSensitive(instance),
	})
	if err != nil {
		return nil, nil, err
	}
	if databaseMessage == nil {
		return nil, nil, nil
	}

	project, err := s.store.GetProjectV2(ctx, &store.FindProjectMessage{ResourceID: &databaseMessage.ProjectID})
	if err != nil {
		return nil, nil, err
	}
	return project, databaseMessage, nil
}

func (*SQLService) getUser(ctx context.Context) (*store.UserMessage, error) {
	user, ok := ctx.Value(common.UserContextKey).(*store.UserMessage)
	if !ok {
		return nil, status.Errorf(codes.Internal, "user not found")
	}
	if user.MemberDeleted {
		return nil, status.Errorf(codes.PermissionDenied, "the user has been deactivated.")
	}
	return user, nil
}

// IsSQLReviewSupported checks the engine type if SQL review supports it.
func IsSQLReviewSupported(dbType storepb.Engine) bool {
	switch dbType {
	case storepb.Engine_POSTGRES, storepb.Engine_MYSQL, storepb.Engine_TIDB, storepb.Engine_MARIADB, storepb.Engine_ORACLE, storepb.Engine_OCEANBASE_ORACLE, storepb.Engine_OCEANBASE, storepb.Engine_SNOWFLAKE, storepb.Engine_DM, storepb.Engine_MSSQL:
		return true
	default:
		return false
	}
}

// encodeToBase64String encodes the statement to base64 string.
func encodeToBase64String(statement string) string {
	base64Encoded := base64.StdEncoding.EncodeToString([]byte(statement))
	return base64Encoded
}

func convertChangeType(t v1pb.CheckRequest_ChangeType) storepb.PlanCheckRunConfig_ChangeDatabaseType {
	switch t {
	case v1pb.CheckRequest_DDL:
		return storepb.PlanCheckRunConfig_DDL
	case v1pb.CheckRequest_DDL_GHOST:
		return storepb.PlanCheckRunConfig_DDL_GHOST
	case v1pb.CheckRequest_DML:
		return storepb.PlanCheckRunConfig_DML
	default:
		return storepb.PlanCheckRunConfig_CHANGE_DATABASE_TYPE_UNSPECIFIED
	}
}

func (s *SQLService) Check(ctx context.Context, request *v1pb.CheckRequest) (*v1pb.CheckResponse, error) {
	if len(request.Statement) > common.MaxSheetCheckSize {
		return nil, status.Errorf(codes.FailedPrecondition, "statement size exceeds maximum allowed size %dKB", common.MaxSheetCheckSize/1024)
	}

	instanceID, databaseName, err := common.GetInstanceDatabaseID(request.Database)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, err.Error())
	}

	instance, err := s.store.GetInstanceV2(ctx, &store.FindInstanceMessage{
		ResourceID: &instanceID,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get instance, error: %v", err)
	}
	if instance == nil {
		return nil, status.Errorf(codes.NotFound, "instance %q not found", instanceID)
	}

	database, err := s.store.GetDatabaseV2(ctx, &store.FindDatabaseMessage{
		InstanceID:   &instanceID,
		DatabaseName: &databaseName,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get database, error: %v", err)
	}
	if database == nil {
		return nil, status.Errorf(codes.NotFound, "database %q not found", request.Database)
	}

	environment, err := s.store.GetEnvironmentV2(ctx, &store.FindEnvironmentMessage{
		ResourceID: &database.EffectiveEnvironmentID,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get environment, error: %v", err)
	}
	if environment == nil {
		return nil, status.Errorf(codes.NotFound, "environment %q not found", database.EffectiveEnvironmentID)
	}

	var overideMetadata *storepb.DatabaseSchemaMetadata
	if request.Metadata != nil {
		overideMetadata, _ = convertV1DatabaseMetadata(request.Metadata)
	}
	_, adviceList, err := s.sqlReviewCheck(ctx, request.Statement, request.ChangeType, environment, instance, database, overideMetadata)
	if err != nil {
		return nil, err
	}

	return &v1pb.CheckResponse{
		Advices: adviceList,
	}, nil
}

// ParseMyBatisMapper parses a MyBatis mapper XML file and returns the multi-SQL statements.
func (*SQLService) ParseMyBatisMapper(_ context.Context, request *v1pb.ParseMyBatisMapperRequest) (*v1pb.ParseMyBatisMapperResponse, error) {
	content := string(request.Content)

	parser := mapperparser.NewParser(content)
	node, err := parser.Parse()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse mybatis mapper: %v", err)
	}

	var stringsBuilder strings.Builder
	if err := node.RestoreSQL(parser.NewRestoreContext().WithRestoreDataNodePlaceholder("@1"), &stringsBuilder); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to restore mybatis mapper: %v", err)
	}

	statement := stringsBuilder.String()
	singleSQLs, err := base.SplitMultiSQL(storepb.Engine_MYSQL, statement)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to split mybatis mapper: %v", err)
	}

	var results []string
	for _, sql := range singleSQLs {
		if sql.Empty {
			continue
		}
		results = append(results, sql.Text)
	}

	return &v1pb.ParseMyBatisMapperResponse{
		Statements: results,
	}, nil
}

// DifferPreview returns the diff preview of the given SQL statement and metadata.
func (*SQLService) DifferPreview(_ context.Context, request *v1pb.DifferPreviewRequest) (*v1pb.DifferPreviewResponse, error) {
	storeSchemaMetadata, _ := convertV1DatabaseMetadata(request.NewMetadata)
	defaultSchema := extractDefaultSchemaForOracleBranch(storepb.Engine(request.Engine), storeSchemaMetadata)
	schema, err := schema.GetDesignSchema(storepb.Engine(request.Engine), defaultSchema, request.OldSchema, storeSchemaMetadata)
	if err != nil {
		return nil, err
	}

	return &v1pb.DifferPreviewResponse{
		Schema: schema,
	}, nil
}
