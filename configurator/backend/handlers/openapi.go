package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jitsucom/jitsu/configurator/common"
	"github.com/jitsucom/jitsu/configurator/destinations"
	"github.com/jitsucom/jitsu/configurator/entities"
	"github.com/jitsucom/jitsu/configurator/jitsu"
	mw "github.com/jitsucom/jitsu/configurator/middleware"
	"github.com/jitsucom/jitsu/configurator/openapi"
	"github.com/jitsucom/jitsu/configurator/ssl"
	"github.com/jitsucom/jitsu/configurator/storages"
	jadapters "github.com/jitsucom/jitsu/server/adapters"
	jauth "github.com/jitsucom/jitsu/server/authorization"
	"github.com/jitsucom/jitsu/server/config"
	jdestinations "github.com/jitsucom/jitsu/server/destinations"
	jdriversbase "github.com/jitsucom/jitsu/server/drivers/base"
	jgeo "github.com/jitsucom/jitsu/server/geo"
	"github.com/jitsucom/jitsu/server/logging"
	"github.com/jitsucom/jitsu/server/random"
	"github.com/jitsucom/jitsu/server/safego"
	jsources "github.com/jitsucom/jitsu/server/sources"
	jsystem "github.com/jitsucom/jitsu/server/system"
	"github.com/jitsucom/jitsu/server/telemetry"
	"github.com/jitsucom/jitsu/server/timestamp"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

const (
	telemetryUsageKey     = "usage"
	getApiKeysErrMsg      = "API keys getting error"
	getDestinationsErrMsg = "Destinations getting error"
	getSourcesErrMsg      = "Sources getting error"
	getProjectErrMsg      = "Project settings getting error"
	configHeaderText      = `Generated by https://cloud.jitsu.com
Documentation: https://jitsu.com/docs

If executed out of our docker container and batch destinations are used, set up events logging
log:
  path: <path to event logs directory>
`
	jsonContentType = "application/json"
)

//stubS3Config is used in generate Jitsu Server yaml config
var stubS3Config = &jadapters.S3Config{
	AccessKeyID: "Please fill this field with your S3 credentials",
	SecretKey:   "Please fill this field with your S3 credentials",
	Bucket:      "Please fill this field with your S3 bucket",
	Region:      "Please fill this field with your S3 region",
}

type Server struct {
	Name *yaml.Node `json:"name" yaml:"name,omitempty"`
}

type Config struct {
	Server       Server                                `json:"server" yaml:"server,omitempty"`
	APIKeys      []*entities.APIKey                    `json:"api_keys" yaml:"api_keys,omitempty"`
	Destinations map[string]*config.DestinationConfig  `json:"destinations" yaml:"destinations,omitempty"`
	Sources      map[string]*jdriversbase.SourceConfig `json:"sources" yaml:"sources,omitempty"`
}

type SystemConfiguration struct {
	SMTP        bool
	SelfHosted  bool
	DockerHUBID string
	Tag         string
	BuiltAt     string
}

type OpenAPI struct {
	Authorizator   Authorizator
	SSOProvider    SSOProvider
	Configurations *storages.ConfigurationsService
	SystemConfig   *SystemConfiguration
	JitsuService   *jitsu.Service
	UpdateExecutor *ssl.UpdateExecutor
	DefaultS3      *jadapters.S3Config
}

func (oa *OpenAPI) GetApiKeysConfiguration(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	start := timestamp.Now()
	if keys, err := oa.Configurations.GetAllAPIKeys(); err != nil {
		mw.BadRequest(ctx, getApiKeysErrMsg, err)
	} else {
		tokens := make([]jauth.Token, len(keys))
		for i, key := range keys {
			tokens[i] = jauth.Token{
				ID:           key.ID,
				ClientSecret: key.ClientSecret,
				ServerSecret: key.ServerSecret,
				Origins:      key.Origins,
			}
		}

		logging.Debugf("APIKeys response in [%.2f] seconds", timestamp.Now().Sub(start).Seconds())
		ctx.JSON(http.StatusOK, &jauth.TokensPayload{Tokens: tokens})
	}
}

func (oa *OpenAPI) GenerateDefaultProjectApiKey(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.ProjectIdRequest
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if req.ProjectID == "" {
		mw.RequiredField(ctx, "projectID")
	} else if !authority.Allow(req.ProjectID) {
		mw.ForbiddenProject(ctx, req.ProjectID)
	} else if err := oa.Configurations.CreateDefaultAPIKey(req.ProjectID); err != nil {
		mw.BadRequest(ctx, "Failed to create default key for project", err)
	} else {
		mw.StatusOk(ctx)
	}
}

func (oa *OpenAPI) BecomeAnotherCloudUser(ctx *gin.Context, params openapi.BecomeAnotherCloudUserParams) {
	if ctx.IsAborted() {
		return
	}

	if authorizator, err := oa.Authorizator.Cloud(); err != nil {
		mw.Unsupported(ctx, err)
	} else if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if !authority.IsAdmin {
		mw.Forbidden(ctx, "Only admins can use this API method")
	} else if params.UserId == "" {
		mw.RequiredField(ctx, "user_id")
	} else if token, err := authorizator.SignInAs(ctx, params.UserId); err != nil {
		mw.BadRequest(ctx, "sign in failed", err)
	} else {
		ctx.JSON(http.StatusOK, token)
	}
}

func (oa *OpenAPI) CreateFreeTierPostgresDatabase(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.ProjectIdRequest
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if req.ProjectID == "" {
		mw.RequiredField(ctx, "projectID")
	} else if !authority.Allow(req.ProjectID) {
		mw.ForbiddenProject(ctx, req.ProjectID)
	} else if database, err := oa.Configurations.CreateDefaultDestination(req.ProjectID); err != nil {
		mw.BadRequest(ctx, "Failed to create a free tier database", err)
	} else {
		ctx.JSON(http.StatusOK, database)
	}
}

func (oa *OpenAPI) GetDestinationsConfiguration(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	start := timestamp.Now()
	if destinationsByProjectID, err := oa.Configurations.GetAllDestinations(); err != nil {
		mw.BadRequest(ctx, getDestinationsErrMsg, err)
	} else if apiKeysPerProjectByID, err := oa.Configurations.GetAllAPIKeysPerProjectByID(); err != nil {
		mw.BadRequest(ctx, getApiKeysErrMsg, err)
	} else if destinationConfigs, err := oa.mapDestinationsConfiguration(apiKeysPerProjectByID, destinationsByProjectID); err != nil {
		mw.BadRequest(ctx, getDestinationsErrMsg, err)
	} else {
		logging.Debugf("Destinations response in [%.2f] seconds", timestamp.Now().Sub(start).Seconds())
		ctx.JSON(http.StatusOK, &jdestinations.Payload{Destinations: destinationConfigs})
	}
}

func (oa *OpenAPI) EvaluateDestinationJSTransformationScript(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.AnyObject
	if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
		return
	} else if field, ok := req.Get("field"); ok && fmt.Sprint(field) == "_transform" {
		var wrapper struct {
			Config *entities.Destination `json:"config"`
		}

		if err := common.DecodeAsJSON(req.AdditionalProperties, &wrapper); err != nil {
			mw.BadRequest(ctx, "Failed to unmarshal destination config", nil)
			return
		} else if wrapper.Config == nil {
			mw.BadRequest(ctx, "Invalid input data", errors.New("config is required field when field = _transform"))
			return
		} else if destinationConfig, err := destinations.MapConfig("evaluate", wrapper.Config, oa.DefaultS3, nil); err != nil {
			mw.BadRequest(ctx, fmt.Sprintf("Failed to map [%s] config to Jitsu format", wrapper.Config.Type), err)
			return
		} else {
			req.Set("config", destinationConfig)
		}
	}

	if reqData, err := req.MarshalJSON(); err != nil {
		mw.BadRequest(ctx, "Failed to marshal request body to json", err)
	} else if serverStatusCode, serverResponse, err := oa.JitsuService.EvaluateExpression(reqData); err != nil {
		mw.BadRequest(ctx, "Failed to get response from Jitsu server", err)
	} else {
		ctx.Data(serverStatusCode, jsonContentType, serverResponse)
	}
}

func (oa *OpenAPI) TestDestinationConfiguration(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req entities.Destination
	if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if destinationConfig, err := destinations.MapConfig("test_connection", &req, oa.DefaultS3, nil); err != nil {
		mw.BadRequest(ctx, fmt.Sprintf("Failed to map [%s] config to Jitsu Server format", req.Type), err)
	} else if destinationData, err := json.Marshal(destinationConfig); err != nil {
		mw.BadRequest(ctx, "Failed to serialize destination config", err)
	} else if serverStatusCode, serverResponse, err := oa.JitsuService.TestDestination(destinationData); err != nil {
		mw.BadRequest(ctx, "Failed to get response from jitsu server", err)
	} else if serverStatusCode == http.StatusOK {
		ctx.JSON(http.StatusOK, &openapi.StatusResponse{Status: "Connection established"})
	} else {
		ctx.Data(serverStatusCode, jsonContentType, serverResponse)
	}
}

func (oa *OpenAPI) GetGeoDataResolvers(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	start := timestamp.Now()
	if geoResolvers, err := oa.Configurations.GetGeoDataResolvers(); err != nil {
		mw.InternalError(ctx, getDestinationsErrMsg, err)
	} else {
		resolverConfigs := make(map[string]*jgeo.ResolverConfig, len(geoResolvers))
		for projectID, geoResolver := range geoResolvers {
			if geoResolver.MaxMind != nil && geoResolver.MaxMind.Enabled {
				maxmindURL := geoResolver.MaxMind.LicenseKey
				if !strings.HasPrefix(maxmindURL, jgeo.MaxmindPrefix) {
					maxmindURL = jgeo.MaxmindPrefix + maxmindURL
				}

				resolverConfigs[projectID] = &jgeo.ResolverConfig{
					Type:   jgeo.MaxmindType,
					Config: jgeo.MaxMindConfig{MaxMindURL: maxmindURL},
				}
			}
		}

		logging.Debugf("Geo data resolvers response in [%.2f] seconds", timestamp.Now().Sub(start).Seconds())
		ctx.JSON(http.StatusOK, &jgeo.Payload{GeoResolvers: resolverConfigs})
	}
}

func (oa *OpenAPI) GenerateJitsuServerYamlConfiguration(ctx *gin.Context, params openapi.GenerateJitsuServerYamlConfigurationParams) {
	if ctx.IsAborted() {
		return
	}

	var (
		project  storages.Project
		response yaml.Node
	)

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(params.ProjectId); projectID == "" {
		mw.RequiredField(ctx, "project_id")
	} else if !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if apiKeys, err := oa.Configurations.GetAPIKeysByProjectID(projectID); err != nil {
		mw.BadRequest(ctx, getApiKeysErrMsg, err)
	} else if projectDestinations, err := oa.Configurations.GetDestinationsByProjectID(projectID); err != nil {
		mw.BadRequest(ctx, getDestinationsErrMsg, err)
	} else if postHandleDestinationIDs, destinationConfigs, err := mapYamlDestinations(projectDestinations); err != nil {
		mw.BadRequest(ctx, "map destination configs", err)
	} else if projectSources, err := oa.Configurations.GetSourcesByProjectID(projectID); err != nil {
		mw.BadRequest(ctx, getSourcesErrMsg, err)
	} else if err := oa.Configurations.Load(projectID, &project); err != nil {
		mw.BadRequest(ctx, getProjectErrMsg, err)
	} else if sourceConfigs, err := mapYamlSources(projectSources, postHandleDestinationIDs, project); err != nil {
		mw.BadRequest(ctx, "map source configs", err)
	} else if data, err := yaml.Marshal(Config{
		Server: Server{
			Name: &yaml.Node{
				Kind:        yaml.ScalarNode,
				Value:       random.String(5),
				LineComment: "rename server if another name is desired",
			},
		},
		APIKeys:      apiKeys,
		Destinations: destinationConfigs,
		Sources:      sourceConfigs,
	}); err != nil {
		mw.InternalError(ctx, "marshal config", err)
	} else if err := yaml.Unmarshal(data, &response); err != nil {
		mw.BadRequest(ctx, "Failed to deserialize result configuration", err)
	} else {
		response.HeadComment = configHeaderText
		ctx.Header("Content-Type", "application/yaml")
		encoder := yaml.NewEncoder(ctx.Writer)
		defer encoder.Close()
		encoder.SetIndent(2)
		if err := encoder.Encode(&response); err != nil {
			mw.BadRequest(ctx, "Failed to write response", err)
		}
	}
}

func (oa *OpenAPI) GetSourcesConfiguration(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	start := timestamp.Now()
	if sourcesByProjectID, err := oa.Configurations.GetAllSources(); err != nil {
		mw.BadRequest(ctx, getSourcesErrMsg, err)
	} else if sourceConfigs, err := oa.mapSourcesConfiguration(sourcesByProjectID); err != nil {
		mw.BadRequest(ctx, getSourcesErrMsg, err)
	} else {
		logging.Debugf("Sources response in [%.2f] seconds", timestamp.Now().Sub(start).Seconds())
		ctx.JSON(http.StatusOK, &jsources.Payload{Sources: sourceConfigs})
	}
}

func (oa *OpenAPI) TestSourceConfiguration(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var source entities.Source
	if err := ctx.BindJSON(&source); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if sourceConfig, err := mapSourceConfig(&source, []string{}, []string{}, openapi.ProjectSettings{}); err != nil {
		mw.BadRequest(ctx, fmt.Sprintf("Failed to map [%s] config to Jitsu Server format", source.SourceType), err)
	} else if sourceConfigData, err := json.Marshal(sourceConfig); err != nil {
		mw.BadRequest(ctx, "Failed to serialize source config", err)
	} else if serverStatusCode, serverResponse, err := oa.JitsuService.TestSource(sourceConfigData); err != nil {
		mw.BadRequest(ctx, "Failed to get response from Jitsu server", err)
	} else if serverStatusCode == http.StatusOK {
		ctx.JSON(http.StatusOK, &openapi.StatusResponse{Status: "Connection established"})
	} else {
		ctx.Data(serverStatusCode, jsonContentType, serverResponse)
	}
}

func (oa *OpenAPI) ReissueProjectSSLCertificates(ctx *gin.Context, params openapi.ReissueProjectSSLCertificatesParams) {
	if ctx.IsAborted() {
		return
	}

	if updater := oa.UpdateExecutor; updater == nil {
		mw.Unsupported(ctx, errSSLNotConfigured)
	} else if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(params.ProjectId); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if params.Async != nil && *params.Async {
		safego.Run(func() {
			if err := updater.RunForProject(projectID); err != nil {
				logging.Errorf("Error updating SSL for project [%s]: %v", projectID, err)
			}
		})

		ctx.JSON(http.StatusOK, &openapi.StatusResponse{Status: "scheduled SSL update"})
	} else if err := updater.RunForProject(projectID); err != nil {
		logging.Errorf("Error updating SSL for project [%s]: %v", projectID, err)
		mw.BadRequest(ctx, fmt.Sprintf("Error running SSL update for project [%s]", projectID), err)
	} else {
		mw.StatusOk(ctx)
	}
}

func (oa *OpenAPI) ReissueAllConfiguredSSLCertificates(ctx *gin.Context, params openapi.ReissueAllConfiguredSSLCertificatesParams) {
	if ctx.IsAborted() {
		return
	}

	if updater := oa.UpdateExecutor; updater == nil {
		mw.Unsupported(ctx, errSSLNotConfigured)
	} else if params.Async != nil && *params.Async {
		safego.Run(func() {
			if err := updater.Run(); err != nil {
				logging.Errorf("Error updating all SSL: %v", err)
			}
		})

		ctx.JSON(http.StatusOK, &openapi.StatusResponse{Status: "scheduled SSL update"})
	} else if err := updater.Run(); err != nil {
		logging.Errorf("Error updating all SSL: %v", err)
		mw.BadRequest(ctx, "Error running SSL update", err)
	} else {
		mw.StatusOk(ctx)
	}
}

func (oa *OpenAPI) GetSystemConfiguration(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	if hasUsers, err := oa.Authorizator.HasUsers(ctx); err != nil {
		mw.InternalError(ctx, "Error checking users existence", err)
	} else if telemetryConfig, err := oa.Configurations.GetParsedTelemetry(); err != nil && !errors.Is(err, storages.ErrConfigurationNotFound) {
		mw.InternalError(ctx, "Error getting telemetry configuration", err)
	} else {
		var telemetryUsageDisabled bool
		if telemetryConfig != nil && telemetryConfig.Disabled != nil {
			usageDisabled, ok := telemetryConfig.Disabled[telemetryUsageKey]
			if ok {
				telemetryUsageDisabled = usageDisabled
			}
		}

		result := jsystem.Configuration{
			Authorization:               oa.Authorizator.AuthorizationType(),
			Users:                       hasUsers,
			SMTP:                        oa.SystemConfig.SMTP,
			SelfHosted:                  oa.SystemConfig.SelfHosted,
			SupportWidget:               !oa.SystemConfig.SelfHosted,
			DefaultS3Bucket:             !oa.SystemConfig.SelfHosted,
			SupportTrackingDomains:      !oa.SystemConfig.SelfHosted,
			TelemetryUsageDisabled:      telemetryUsageDisabled,
			ShowBecomeUser:              !oa.SystemConfig.SelfHosted,
			DockerHubID:                 oa.SystemConfig.DockerHUBID,
			OnlyAdminCanChangeUserEmail: oa.SystemConfig.SelfHosted,
			Tag:                         oa.SystemConfig.Tag,
			BuiltAt:                     oa.SystemConfig.BuiltAt,
		}

		if oa.SSOProvider != nil {
			result.SSOAuthLink = oa.SSOProvider.AuthLink()
		}

		ctx.JSON(http.StatusOK, result)
	}
}

func (oa *OpenAPI) GetSystemVersion(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	ctx.JSON(http.StatusOK, openapi.VersionObject{
		Version: oa.SystemConfig.Tag,
		BuiltAt: oa.SystemConfig.BuiltAt,
	})
}

func (oa *OpenAPI) GetTelemetrySettings(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var result json.RawMessage
	if data, err := oa.Configurations.GetTelemetry(); errors.Is(err, storages.ErrConfigurationNotFound) {
		result = json.RawMessage("{}")
	} else if err != nil {
		mw.BadRequest(ctx, "error getting telemetry configuration", err)
		return
	} else {
		result = data
	}

	ctx.Data(http.StatusOK, jsonContentType, result)
}

func (oa *OpenAPI) UserEmailChange(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.UserEmailChangeJSONBody
	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if req.OldEmail == "" {
		mw.RequiredField(ctx, "old_email")
	} else if req.NewEmail == "" {
		mw.RequiredField(ctx, "new_email")
	} else if userID, err := authorizator.ChangeEmail(ctx, req.OldEmail, req.NewEmail); err != nil {
		mw.BadRequest(ctx, "email update failed", err)
	} else if _, err := oa.Configurations.UpdateUserInfo(userID, mw.UserInfoEmailUpdate{Email: req.NewEmail}); err != nil {
		mw.BadRequest(ctx, "update user info", err)
	} else {
		mw.StatusOk(ctx)
	}
}

func (oa *OpenAPI) GetUserInfo(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var result storages.RedisUserInfo
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if authority.IsAnonymous() {
		mw.Forbidden(ctx, "user must be non-anonymous")
	} else if err := oa.Configurations.Load(authority.UserInfo.Id, &result); errors.Is(err, storages.ErrConfigurationNotFound) {
		ctx.Data(http.StatusOK, jsonContentType, []byte("{}"))
	} else if err != nil {
		mw.BadRequest(ctx, "load user info", err)
	} else {
		ctx.JSON(http.StatusOK, result)
	}
}

func (oa *OpenAPI) UpdateUserInfo(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.UpdateUserInfoRequest
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if authority.IsAnonymous() {
		mw.Forbidden(ctx, "user must be non-anonymous")
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if result, err := oa.Configurations.UpdateUserInfo(authority.UserInfo.Id, req); err != nil {
		mw.BadRequest(ctx, "patch user info failed", err)
	} else {
		ctx.JSON(http.StatusOK, result)
	}
}

func (oa *OpenAPI) UserSignUp(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.UserSignUpJSONRequestBody
	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if req.Email == "" {
		mw.RequiredField(ctx, "email")
	} else if req.Password == "" {
		mw.RequiredField(ctx, "password")
	} else /*if !req.EmailOptout && req.Name == "" {
		mw.RequiredField(ctx, "name")
	} else if !req.EmailOptout && req.Company == "" {
		mw.RequiredField(ctx, "company")
	} else */if tokenPair, err := authorizator.SignUp(ctx, req.Email, req.Password); err != nil {
		mw.BadRequest(ctx, "sign up failed", err)
	} else {
		if err := oa.Configurations.SaveTelemetry(map[string]bool{telemetryUsageKey: req.UsageOptout}); err != nil {
			logging.Errorf("Error saving telemetry configuration [%v] to storage: %v", req.UsageOptout, err)
		}

		userData := &telemetry.UserData{
			Company:     req.Company,
			EmailOptout: req.EmailOptout,
			UsageOptout: req.UsageOptout,
		}

		if !req.EmailOptout {
			userData.Email = req.Email
			userData.Name = req.Name
		}

		telemetry.User(userData)

		ctx.JSON(http.StatusOK, tokenPair)
	}
}

func (oa *OpenAPI) UserPasswordChange(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.UserPasswordChangeJSONBody
	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else {
		var tokenPair *openapi.TokensResponse
		if req.ResetId != nil && *req.ResetId != "" {
			tokenPair, err = authorizator.ResetPassword(ctx, *req.ResetId, req.NewPassword)
		} else if token := mw.GetToken(ctx); ctx.IsAborted() {
			return
		} else {
			tokenPair, err = authorizator.ChangePassword(ctx, token, req.NewPassword)
		}

		if err != nil {
			mw.BadRequest(ctx, "update password", err)
		} else {
			ctx.JSON(http.StatusOK, tokenPair)
		}
	}
}

func (oa *OpenAPI) UserPasswordReset(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.UserPasswordResetJSONRequestBody
	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if req.Email == "" {
		mw.RequiredField(ctx, "email")
	} else if req.Callback == nil || *req.Callback == "" {
		mw.RequiredField(ctx, "callback")
	} else if err := authorizator.SendResetPasswordLink(ctx, req.Email, *req.Callback); err != nil {
		mw.BadRequest(ctx, "send reset password link", err)
	} else {
		mw.StatusOk(ctx)
	}
}

func (oa *OpenAPI) UserSignIn(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.UserSignInJSONRequestBody
	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if req.Email == "" {
		mw.RequiredField(ctx, "email")
	} else if req.Password == "" {
		mw.RequiredField(ctx, "password")
	} else if tokenPair, err := authorizator.SignIn(ctx, req.Email, req.Password); err != nil {
		mw.BadRequest(ctx, "sign in failed", err)
	} else {
		ctx.JSON(http.StatusOK, tokenPair)
	}
}

func (oa *OpenAPI) UserSignOut(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	if token := mw.GetToken(ctx); ctx.IsAborted() {
		return
	} else if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := authorizator.SignOut(ctx, token); err != nil {
		mw.BadRequest(ctx, "sign out failed", err)
	} else {
		mw.StatusOk(ctx)
	}
}

func (oa *OpenAPI) UserAuthorizationTokenRefresh(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.UserAuthorizationTokenRefreshJSONRequestBody
	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if req.RefreshToken == "" {
		mw.RequiredField(ctx, "refresh_token")
	} else if tokenPair, err := authorizator.RefreshToken(ctx, req.RefreshToken); err != nil {
		mw.BadRequest(ctx, "refresh token failed", err)
	} else {
		ctx.JSON(http.StatusOK, tokenPair)
	}
}

func (oa *OpenAPI) GetObjectsByProjectIdAndObjectType(ctx *gin.Context, projectID openapi.ProjectId, objectType openapi.ObjectType) {
	if ctx.IsAborted() {
		return
	}

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else {
		objectType := string(objectType)
		if objectsData, err := oa.Configurations.GetConfigWithLock(objectType, projectID); errors.Is(err, storages.ErrConfigurationNotFound) {
			ctx.Data(http.StatusOK, jsonContentType, []byte("[]"))
		} else if err != nil {
			mw.BadRequest(ctx, fmt.Sprintf("failed to get objects for object type=[%s], projectID=[%s]", objectType, projectID), err)
		} else if objectsPath := oa.Configurations.GetObjectArrayPathByObjectType(objectType); objectsPath == "" {
			ctx.Data(http.StatusOK, jsonContentType, objectsData)
		} else {
			objectsValue := make(map[string]json.RawMessage)
			if err := json.Unmarshal(objectsData, &objectsValue); err != nil {
				mw.BadRequest(ctx, fmt.Sprintf("failed to deserialize objects for object type=[%s], projectID=[%s]", objectType, projectID), err)
			} else if objects, ok := objectsValue[objectsPath]; !ok {
				mw.BadRequest(ctx, fmt.Sprintf("failed to read %s objects node for object type=[%s], projectID=[%s]", objectsPath, objectType, projectID), err)
			} else {
				ctx.Data(http.StatusOK, jsonContentType, objects)
			}
		}
	}
}

func (oa *OpenAPI) CreateObjectInProject(ctx *gin.Context, projectID openapi.ProjectId, objectType openapi.ObjectType) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.AnyObject
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unsupported(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if newObject, err := oa.Configurations.CreateObjectWithLock(string(objectType), projectID, &req); err != nil {
		mw.BadRequest(ctx, fmt.Sprintf("failed to create object [%s], project id=[%s]", objectType, projectID), err)
	} else {
		ctx.Data(http.StatusOK, jsonContentType, newObject)
	}
}

func (oa *OpenAPI) DeleteObjectByUid(ctx *gin.Context, projectID openapi.ProjectId, objectType openapi.ObjectType, objectUID openapi.ObjectUid) {
	if ctx.IsAborted() {
		return
	}

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else {
		objectType, objectUID := string(objectType), string(objectUID)
		payload := &storages.PatchPayload{
			ObjectArrayPath: oa.Configurations.GetObjectArrayPathByObjectType(objectType),
			ObjectMeta: &storages.ObjectMeta{
				IDFieldPath: oa.Configurations.GetObjectIDField(objectType),
				Value:       objectUID,
			},
			Patch: nil,
		}

		if deletedObject, err := oa.Configurations.DeleteObjectWithLock(objectType, projectID, payload); err != nil {
			mw.BadRequest(ctx, fmt.Sprintf("failed to delete object [%s] in project [%s], id=[%s]", objectType, projectID, objectUID), err)
		} else {
			ctx.Data(http.StatusOK, jsonContentType, deletedObject)
		}
	}
}

func (oa *OpenAPI) GetObjectByUid(ctx *gin.Context, projectID openapi.ProjectId, objectType openapi.ObjectType, objectUID openapi.ObjectUid) {
	if ctx.IsAborted() {
		return
	}

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else {
		objectType, objectUID := string(objectType), string(objectUID)
		objectArrayPath := oa.Configurations.GetObjectArrayPathByObjectType(objectType)
		objectMeta := &storages.ObjectMeta{
			IDFieldPath: oa.Configurations.GetObjectIDField(objectType),
			Value:       objectUID,
		}

		if object, err := oa.Configurations.GetObjectWithLock(objectType, projectID, objectArrayPath, objectMeta); err != nil {
			mw.BadRequest(ctx, fmt.Sprintf("failed to get object [%s] in project [%s], id=[%s]", objectType, projectID, objectUID), err)
		} else {
			ctx.Data(http.StatusOK, jsonContentType, object)
		}
	}
}

func (oa *OpenAPI) PatchObjectByUid(ctx *gin.Context, projectID openapi.ProjectId, objectType openapi.ObjectType, objectUID openapi.ObjectUid) {
	if ctx.IsAborted() {
		return
	}

	req := make(map[string]interface{})
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else {
		objectType, objectUID := string(objectType), string(objectUID)
		patch := &storages.PatchPayload{
			ObjectArrayPath: oa.Configurations.GetObjectArrayPathByObjectType(objectType),
			ObjectMeta: &storages.ObjectMeta{
				IDFieldPath: oa.Configurations.GetObjectIDField(objectType),
				Value:       objectUID,
			},
			Patch: req,
		}

		if newObject, err := oa.Configurations.PatchObjectWithLock(objectType, projectID, patch); err != nil {
			mw.BadRequest(ctx, fmt.Sprintf("failed to patch object [%s] in project [%s], id=[%s]", objectType, projectID, objectUID), err)
		} else {
			ctx.Data(http.StatusOK, jsonContentType, newObject)
		}
	}
}

func (oa *OpenAPI) ReplaceObjectByUid(ctx *gin.Context, projectID openapi.ProjectId, objectType openapi.ObjectType, objectUID openapi.ObjectUid) {
	if ctx.IsAborted() {
		return
	}

	req := make(map[string]interface{})
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else {
		objectType, objectUID := string(objectType), string(objectUID)
		patch := &storages.PatchPayload{
			ObjectArrayPath: oa.Configurations.GetObjectArrayPathByObjectType(objectType),
			ObjectMeta: &storages.ObjectMeta{
				IDFieldPath: oa.Configurations.GetObjectIDField(objectType),
				Value:       objectUID,
			},
			Patch: req,
		}

		if newObject, err := oa.Configurations.ReplaceObjectWithLock(objectType, projectID, patch); err != nil {
			mw.BadRequest(ctx, fmt.Sprintf("failed to patch object [%s] in project [%s], id=[%s]", objectType, projectID, objectUID), err)
		} else {
			ctx.Data(http.StatusOK, jsonContentType, newObject)
		}
	}
}

func (oa *OpenAPI) GetProjectSettings(ctx *gin.Context, projectID openapi.ProjectId) {
	if ctx.IsAborted() {
		return
	}

	var result storages.Project
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if err := oa.Configurations.Load(projectID, &result); err != nil {
		mw.BadRequest(ctx, "get project settings failed", err)
	} else {
		ctx.JSON(http.StatusOK, result.ProjectSettings)
	}
}

func (oa *OpenAPI) PatchProjectSettings(ctx *gin.Context, projectID openapi.ProjectId) {
	if ctx.IsAborted() {
		return
	}

	var (
		req    openapi.ProjectSettings
		result storages.Project
	)

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if projectID := string(projectID); !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if _, err := oa.Configurations.Patch(projectID, &result, req, false); err != nil {
		mw.BadRequest(ctx, "patch project settings failed", err)
	} else {
		ctx.JSON(http.StatusOK, result.ProjectSettings)
	}
}

func (oa *OpenAPI) LinkUserToProject(ctx *gin.Context, projectID string) {
	if ctx.IsAborted() {
		return
	}

	var req openapi.LinkProjectRequest
	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
		return
	} else if !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
		return
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
		return
	}

	var (
		userID     string
		userStatus = "created"
	)

	if req.UserId != nil && *req.UserId != "" {
		userID = *req.UserId
	} else if req.UserEmail != nil && *req.UserEmail != "" {
		var err error
		if userID, err = oa.Authorizator.AutoSignUp(ctx, *req.UserEmail, req.Callback); errors.Is(err, ErrUserExists) {
			userStatus = "existing"
		} else if err != nil {
			mw.BadRequest(ctx, "auto sign up failed", err)
			return
		}
	} else {
		mw.RequiredField(ctx, "either userId or userEmail")
		return
	}

	if err := oa.Configurations.LinkUserToProject(userID, projectID); err != nil {
		mw.BadRequest(ctx, "link user to project failed", err)
	} else if users, err := oa.getProjectUsers(ctx, projectID); err != nil {
		mw.BadRequest(ctx, "get project users", err)
	} else {
		ctx.JSON(http.StatusOK, openapi.LinkProjectResponse{
			ProjectUsers: users,
			UserStatus:   userStatus,
		})
	}
}

func (oa *OpenAPI) UnlinkUserFromProject(ctx *gin.Context, projectID string, params openapi.UnlinkUserFromProjectParams) {
	if ctx.IsAborted() {
		return
	}

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if err := oa.Configurations.UnlinkUserFromProject(params.UserId, projectID); err != nil {
		mw.BadRequest(ctx, "unlink user from project failed", err)
	} else {
		ctx.JSON(http.StatusOK, mw.OkResponse)
	}
}

func (oa *OpenAPI) GetUsersLinkToProjects(ctx *gin.Context, projectID string) {
	if ctx.IsAborted() {
		return
	}

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if !authority.Allow(projectID) {
		mw.ForbiddenProject(ctx, projectID)
	} else if users, err := oa.getProjectUsers(ctx, projectID); err != nil {
		mw.BadRequest(ctx, "get project users failed", err)
	} else {
		ctx.JSON(http.StatusOK, users)
	}
}

func (oa *OpenAPI) GetProjects(ctx *gin.Context, params openapi.GetProjectsParams) {
	if ctx.IsAborted() {
		return
	}

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if authority.IsAdmin && (authority.IsAnonymous() || params.AllProjects != nil && *params.AllProjects) {
		if projects, err := oa.Configurations.GetAllProjects(); err != nil {
			mw.InternalError(ctx, "get all projects failed", err)
		} else {
			ctx.JSON(http.StatusOK, projects)
		}
	} else if params.AllProjects != nil && *params.AllProjects {
		mw.Forbidden(ctx, "admin required")
	} else {
		projects := make([]openapi.Project, 0, len(authority.ProjectIDs))
		for projectID := range authority.ProjectIDs {
			var project storages.Project
			if err := oa.Configurations.Load(projectID, &project); errors.Is(err, storages.ErrConfigurationNotFound) {
				continue
			} else if err != nil {
				mw.BadRequest(ctx, fmt.Sprintf("get project %s failed", projectID), err)
				return
			} else {
				projects = append(projects, project.Project)
			}
		}

		ctx.JSON(http.StatusOK, projects)
	}
}

func (oa *OpenAPI) CreateProjectAndLinkUser(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	var (
		req    openapi.CreateProjectRequest
		result storages.Project
	)

	if authority, err := mw.GetAuthority(ctx); err != nil {
		mw.Unauthorized(ctx, err)
	} else if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
	} else if err := oa.Configurations.Create(&result, req); err != nil {
		mw.BadRequest(ctx, "create project failed", err)
	} else {
		if !authority.IsAnonymous() {
			if err := oa.Configurations.LinkUserToProject(authority.UserInfo.Id, result.Id); err != nil {
				mw.BadRequest(ctx, "link user to project failed", err)
				return
			}
		}

		ctx.JSON(http.StatusOK, result.Project)
	}
}

func (oa *OpenAPI) ListUsers(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if users, err := authorizator.ListUsers(ctx); err != nil {
		mw.BadRequest(ctx, "list users failed", err)
	} else {
		ctx.JSON(http.StatusOK, users)
	}
}

func (oa *OpenAPI) CreateNewUser(ctx *gin.Context) {
	if ctx.IsAborted() {
		return
	}

	authorizator, err := oa.Authorizator.Local()
	if err != nil {
		mw.Unsupported(ctx, err)
		return
	}

	var req openapi.CreateUserRequest
	if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
		return
	} else if req.ProjectId == nil && req.ProjectName == nil {
		mw.RequiredField(ctx, "either projectId or projectName")
		return
	}

	var project storages.Project
	if req.ProjectId != nil {
		if err := oa.Configurations.Load(*req.ProjectId, &project); err != nil {
			mw.BadRequest(ctx, "load project by id", err)
			return
		}
	} else if err := oa.Configurations.Create(&project, openapi.CreateProjectRequest{Name: *req.ProjectName}); err != nil {
		mw.BadRequest(ctx, "create new project with name", err)
		return
	}

	createdUser, err := authorizator.CreateUser(ctx, req.Email)
	if err != nil {
		mw.BadRequest(ctx, "failed to create user", err)
		return
	}

	userInfo, err := oa.Configurations.UpdateUserInfo(createdUser.ID, openapi.UpdateUserInfoRequest{
		Name: common.NilOrString(req.Name),
		Project: &openapi.ProjectInfoUpdate{
			Id:           &project.Id,
			Name:         &project.Name,
			RequireSetup: project.RequiresSetup,
		},
	})
	if err != nil {
		mw.BadRequest(ctx, "update user info", err)
		return
	}

	ctx.JSON(http.StatusOK, openapi.CreateUserResponse{
		User: openapi.User{
			UserBasicInfo: openapi.UserBasicInfo{
				Id:    createdUser.ID,
				Email: req.Email,
			},
			Created: userInfo.Created,
			Name:    req.Name,
		},
		Project: project.Project,
		ResetId: createdUser.ResetID,
	})
}

func (oa *OpenAPI) DeleteUser(ctx *gin.Context, userID string) {
	if ctx.IsAborted() {
		return
	}

	if authorizator, err := oa.Authorizator.Local(); err != nil {
		mw.Unsupported(ctx, err)
	} else if err := authorizator.DeleteUser(ctx, userID); err != nil {
		mw.BadRequest(ctx, "delete user failed", err)
	} else {
		if err := oa.Configurations.Delete(userID, new(storages.RedisUserInfo)); err != nil {
			logging.Warnf("failed to delete user info for id %s: %s", userID, err)
		}

		if err := oa.Configurations.UnlinkUserFromAllProjects(userID); err != nil {
			logging.Warnf("failed to unlink user %s from all projects: %s", err)
		}

		mw.StatusOk(ctx)
	}
}

func (oa *OpenAPI) UpdateUser(ctx *gin.Context, userID string) {
	if ctx.IsAborted() {
		return
	}

	email, err := oa.Authorizator.GetUserEmail(ctx, userID)
	if err != nil {
		mw.BadRequest(ctx, "get user email", err)
		return
	}

	var req openapi.PatchUserRequest
	if err := ctx.BindJSON(&req); err != nil {
		mw.InvalidInputJSON(ctx, err)
		return
	}

	if req.Password != nil {
		if authorizator, err := oa.Authorizator.Local(); err != nil {
			mw.Unsupported(ctx, err)
			return
		} else if err := authorizator.UpdatePassword(ctx, userID, *req.Password); err != nil {
			mw.BadRequest(ctx, "update user failed", err)
			return
		}
	}

	if userInfo, err := oa.Configurations.UpdateUserInfo(userID, openapi.UpdateUserInfoRequest{
		Name:                common.NilOrString(req.Name),
		ForcePasswordChange: req.ForcePasswordChange,
	}); err != nil {
		mw.BadRequest(ctx, "update user info", err)
	} else {
		result := &openapi.User{
			UserBasicInfo: openapi.UserBasicInfo{
				Id:    userID,
				Email: email,
			},
			EmailOptout:         userInfo.EmailOptout,
			ForcePasswordChange: userInfo.ForcePasswordChange,
			Name:                userInfo.Name,
			Created:             userInfo.Created,
		}

		if suggestedInfo := userInfo.SuggestedInfo; suggestedInfo != nil {
			result.SuggestedCompanyName = suggestedInfo.CompanyName
		}

		ctx.JSON(http.StatusOK, result)
	}
}
