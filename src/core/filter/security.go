// Copyright 2018 Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package filter

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	beegoctx "github.com/astaxie/beego/context"
	"github.com/docker/distribution/reference"
	"github.com/goharbor/harbor/src/common"
	"github.com/goharbor/harbor/src/common/dao"
	"github.com/goharbor/harbor/src/common/models"
	secstore "github.com/goharbor/harbor/src/common/secret"
	"github.com/goharbor/harbor/src/common/security"
	admr "github.com/goharbor/harbor/src/common/security/admiral"
	"github.com/goharbor/harbor/src/common/security/admiral/authcontext"
	"github.com/goharbor/harbor/src/common/security/local"
	robotCtx "github.com/goharbor/harbor/src/common/security/robot"
	"github.com/goharbor/harbor/src/common/security/secret"
	"github.com/goharbor/harbor/src/common/token"
	"github.com/goharbor/harbor/src/common/utils/log"
	"github.com/goharbor/harbor/src/core/auth"
	"github.com/goharbor/harbor/src/core/config"
	"github.com/goharbor/harbor/src/core/promgr"
	"github.com/goharbor/harbor/src/core/promgr/pmsdriver/admiral"
	"strings"

	"encoding/json"
	k8s_api_v1beta1 "k8s.io/api/authentication/v1beta1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

// ContextValueKey for content value
type ContextValueKey string

type pathMethod struct {
	path   string
	method string
}

const (
	// SecurCtxKey is context value key for security context
	SecurCtxKey ContextValueKey = "harbor_security_context"

	// PmKey is context value key for the project manager
	PmKey ContextValueKey = "harbor_project_manager"
)

var (
	reqCtxModifiers []ReqCtxModifier
	// basic auth request context modifier only takes effect on the patterns
	// in the slice
	basicAuthReqPatterns = []*pathMethod{
		// create project
		{
			path:   "/api/projects",
			method: http.MethodPost,
		},
		// token service
		{
			path:   "/service/token",
			method: http.MethodGet,
		},
		// delete repository
		{
			path:   "/api/repositories/" + reference.NameRegexp.String(),
			method: http.MethodDelete,
		},
		// delete tag
		{
			path:   "/api/repositories/" + reference.NameRegexp.String() + "/tags/" + reference.TagRegexp.String(),
			method: http.MethodDelete,
		},
	}
)

// Init ReqCtxMofiers list
func Init() {
	// integration with admiral
	if config.WithAdmiral() {
		reqCtxModifiers = []ReqCtxModifier{
			&secretReqCtxModifier{config.SecretStore},
			&tokenReqCtxModifier{},
			&basicAuthReqCtxModifier{},
			&unauthorizedReqCtxModifier{}}
		return
	}

	// standalone
	reqCtxModifiers = []ReqCtxModifier{
		&secretReqCtxModifier{config.SecretStore},
		&authProxyReqCtxModifier{},
		&robotAuthReqCtxModifier{},
		&basicAuthReqCtxModifier{},
		&sessionReqCtxModifier{},
		&unauthorizedReqCtxModifier{}}
}

// SecurityFilter authenticates the request and passes a security context
// and a project manager with it which can be used to do some authN & authZ
func SecurityFilter(ctx *beegoctx.Context) {
	if ctx == nil {
		return
	}

	req := ctx.Request
	if req == nil {
		return
	}

	// add security context and project manager to request context
	for _, modifier := range reqCtxModifiers {
		if modifier.Modify(ctx) {
			break
		}
	}
}

// ReqCtxModifier modifies the context of request
type ReqCtxModifier interface {
	Modify(*beegoctx.Context) bool
}

type secretReqCtxModifier struct {
	store *secstore.Store
}

func (s *secretReqCtxModifier) Modify(ctx *beegoctx.Context) bool {
	scrt := secstore.FromRequest(ctx.Request)
	if len(scrt) == 0 {
		return false
	}
	log.Debug("got secret from request")

	log.Debug("using global project manager")
	pm := config.GlobalProjectMgr

	log.Debug("creating a secret security context...")
	securCtx := secret.NewSecurityContext(scrt, s.store)

	setSecurCtxAndPM(ctx.Request, securCtx, pm)

	return true
}

type robotAuthReqCtxModifier struct{}

func (r *robotAuthReqCtxModifier) Modify(ctx *beegoctx.Context) bool {
	robotName, robotTk, ok := ctx.Request.BasicAuth()
	if !ok {
		return false
	}
	if !strings.HasPrefix(robotName, common.RobotPrefix) {
		return false
	}
	rClaims := &token.RobotClaims{}
	htk, err := token.ParseWithClaims(robotTk, rClaims)
	if err != nil {
		log.Errorf("failed to decrypt robot token, %v", err)
		return false
	}
	// Do authn for robot account, as Harbor only stores the token ID, just validate the ID and disable.
	robot, err := dao.GetRobotByID(htk.Claims.(*token.RobotClaims).TokenID)
	if err != nil {
		log.Errorf("failed to get robot %s: %v", robotName, err)
		return false
	}
	if robot == nil {
		log.Error("the token provided doesn't exist.")
		return false
	}
	if robotName != robot.Name {
		log.Errorf("failed to authenticate : %v", robotName)
		return false
	}
	if robot.Disabled {
		log.Errorf("the robot account %s is disabled", robot.Name)
		return false
	}
	log.Debug("creating robot account security context...")
	pm := config.GlobalProjectMgr
	securCtx := robotCtx.NewSecurityContext(robot, pm, htk.Claims.(*token.RobotClaims).Access)
	setSecurCtxAndPM(ctx.Request, securCtx, pm)
	return true
}

type authProxyReqCtxModifier struct{}

func (ap *authProxyReqCtxModifier) Modify(ctx *beegoctx.Context) bool {
	authMode, err := config.AuthMode()
	if err != nil {
		log.Errorf("fail to get auth mode, %v", err)
		return false
	}
	if authMode != common.HTTPAuth {
		return false
	}

	// only support docker login
	if ctx.Request.URL.Path != "/service/token" {
		log.Debug("Auth proxy modifier only handles docker login request.")
		return false
	}

	proxyUserName, proxyPwd, ok := ctx.Request.BasicAuth()
	if !ok {
		return false
	}

	rawUserName, match := ap.matchAuthProxyUserName(proxyUserName)
	if !match {
		log.Errorf("User name %s doesn't meet the auth proxy name pattern", proxyUserName)
		return false
	}

	httpAuthProxyConf, err := config.HTTPAuthProxySetting()
	if err != nil {
		log.Errorf("fail to get auth proxy settings, %v", err)
		return false
	}

	// Init auth client with the auth proxy endpoint.
	authClientCfg := &rest.Config{
		Host: httpAuthProxyConf.TokenReviewEndpoint,
		ContentConfig: rest.ContentConfig{
			GroupVersion:         &schema.GroupVersion{},
			NegotiatedSerializer: serializer.DirectCodecFactory{CodecFactory: scheme.Codecs},
		},
		BearerToken: proxyPwd,
		TLSClientConfig: rest.TLSClientConfig{
			Insecure: httpAuthProxyConf.SkipCertVerify,
		},
	}
	authClient, err := rest.RESTClientFor(authClientCfg)
	if err != nil {
		log.Errorf("fail to create auth client, %v", err)
		return false
	}

	// Do auth with the token.
	tokenReviewRequest := &k8s_api_v1beta1.TokenReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "TokenReview",
			APIVersion: "authentication.k8s.io/v1beta1",
		},
		Spec: k8s_api_v1beta1.TokenReviewSpec{
			Token: proxyPwd,
		},
	}
	res := authClient.Post().Body(tokenReviewRequest).Do()
	err = res.Error()
	if err != nil {
		log.Errorf("fail to POST auth request, %v", err)
		return false
	}
	resRaw, err := res.Raw()
	if err != nil {
		log.Errorf("fail to get raw data of token review, %v", err)
		return false
	}

	// Parse the auth response, check the user name and authenticated status.
	tokenReviewResponse := &k8s_api_v1beta1.TokenReview{}
	err = json.Unmarshal(resRaw, &tokenReviewResponse)
	if err != nil {
		log.Errorf("fail to decode token review, %v", err)
		return false
	}
	if !tokenReviewResponse.Status.Authenticated {
		log.Errorf("fail to auth user: %s", rawUserName)
		return false
	}
	user, err := dao.GetUser(models.User{
		Username: rawUserName,
	})
	if err != nil {
		log.Errorf("fail to get user: %v", err)
		return false
	}
	if user == nil {
		log.Errorf("User: %s has not been on boarded yet.", rawUserName)
		return false
	}
	if rawUserName != tokenReviewResponse.Status.User.Username {
		log.Errorf("user name doesn't match with token: %s", rawUserName)
		return false
	}

	log.Debug("using local database project manager")
	pm := config.GlobalProjectMgr
	log.Debug("creating local database security context for auth proxy...")
	securCtx := local.NewSecurityContext(user, pm)
	setSecurCtxAndPM(ctx.Request, securCtx, pm)
	return true
}

func (ap *authProxyReqCtxModifier) matchAuthProxyUserName(name string) (string, bool) {
	if !strings.HasPrefix(name, common.AuthProxyUserNamePrefix) {
		return "", false
	}
	return strings.Replace(name, common.AuthProxyUserNamePrefix, "", -1), true
}

type basicAuthReqCtxModifier struct{}

func (b *basicAuthReqCtxModifier) Modify(ctx *beegoctx.Context) bool {
	username, password, ok := ctx.Request.BasicAuth()
	if !ok {
		return false
	}
	log.Debug("got user information via basic auth")

	// integration with admiral
	if config.WithAdmiral() {
		// Can't get a token from Admiral's login API, we can only
		// create a project manager with the token of the solution user.
		// That way may cause some wrong permission promotion in some API
		// calls, so we just handle the requests which are necessary
		match := false
		var err error
		path := ctx.Request.URL.Path
		for _, pattern := range basicAuthReqPatterns {
			match, err = regexp.MatchString(pattern.path, path)
			if err != nil {
				log.Errorf("failed to match %s with pattern %s", path, pattern)
				continue
			}
			if match {
				break
			}
		}
		if !match {
			log.Debugf("basic auth is not supported for request %s %s, skip",
				ctx.Request.Method, ctx.Request.URL.Path)
			return false
		}

		token, err := config.TokenReader.ReadToken()
		if err != nil {
			log.Errorf("failed to read solution user token: %v", err)
			return false
		}
		authCtx, err := authcontext.Login(config.AdmiralClient,
			config.AdmiralEndpoint(), username, password, token)
		if err != nil {
			log.Errorf("failed to authenticate %s: %v", username, err)
			return false
		}

		log.Debug("using global project manager...")
		pm := config.GlobalProjectMgr
		log.Debug("creating admiral security context...")
		securCtx := admr.NewSecurityContext(authCtx, pm)

		setSecurCtxAndPM(ctx.Request, securCtx, pm)
		return true
	}

	// standalone
	user, err := auth.Login(models.AuthModel{
		Principal: username,
		Password:  password,
	})
	if err != nil {
		log.Errorf("failed to authenticate %s: %v", username, err)
		return false
	}
	if user == nil {
		log.Debug("basic auth user is nil")
		return false
	}
	log.Debug("using local database project manager")
	pm := config.GlobalProjectMgr
	log.Debug("creating local database security context...")
	securCtx := local.NewSecurityContext(user, pm)
	setSecurCtxAndPM(ctx.Request, securCtx, pm)
	return true
}

type sessionReqCtxModifier struct{}

func (s *sessionReqCtxModifier) Modify(ctx *beegoctx.Context) bool {
	var user models.User
	userInterface := ctx.Input.Session("user")

	if userInterface == nil {
		log.Debug("can not get user information from session")
		return false
	}

	log.Debug("got user information from session")
	user, ok := userInterface.(models.User)
	if !ok {
		log.Info("can not get user information from session")
		return false
	}
	log.Debug("using local database project manager")
	pm := config.GlobalProjectMgr
	log.Debug("creating local database security context...")
	securCtx := local.NewSecurityContext(&user, pm)

	setSecurCtxAndPM(ctx.Request, securCtx, pm)

	return true
}

type tokenReqCtxModifier struct{}

func (t *tokenReqCtxModifier) Modify(ctx *beegoctx.Context) bool {
	token := ctx.Request.Header.Get(authcontext.AuthTokenHeader)
	if len(token) == 0 {
		return false
	}

	log.Debug("got token from request")

	authContext, err := authcontext.GetAuthCtx(config.AdmiralClient,
		config.AdmiralEndpoint(), token)
	if err != nil {
		log.Errorf("failed to get auth context: %v", err)
		return false
	}

	log.Debug("creating PMS project manager...")
	driver := admiral.NewDriver(config.AdmiralClient,
		config.AdmiralEndpoint(), &admiral.RawTokenReader{
			Token: token,
		})

	pm := promgr.NewDefaultProjectManager(driver, false)

	log.Debug("creating admiral security context...")
	securCtx := admr.NewSecurityContext(authContext, pm)
	setSecurCtxAndPM(ctx.Request, securCtx, pm)

	return true
}

// use this one as the last modifier in the modifier list for unauthorized request
type unauthorizedReqCtxModifier struct{}

func (u *unauthorizedReqCtxModifier) Modify(ctx *beegoctx.Context) bool {
	log.Debug("user information is nil")

	var securCtx security.Context
	var pm promgr.ProjectManager
	if config.WithAdmiral() {
		// integration with admiral
		log.Debug("creating PMS project manager...")
		driver := admiral.NewDriver(config.AdmiralClient,
			config.AdmiralEndpoint(), nil)
		pm = promgr.NewDefaultProjectManager(driver, false)
		log.Debug("creating admiral security context...")
		securCtx = admr.NewSecurityContext(nil, pm)
	} else {
		// standalone
		log.Debug("using local database project manager")
		pm = config.GlobalProjectMgr
		log.Debug("creating local database security context...")
		securCtx = local.NewSecurityContext(nil, pm)
	}
	setSecurCtxAndPM(ctx.Request, securCtx, pm)
	return true
}

func setSecurCtxAndPM(req *http.Request, ctx security.Context, pm promgr.ProjectManager) {
	addToReqContext(req, SecurCtxKey, ctx)
	addToReqContext(req, PmKey, pm)
}

func addToReqContext(req *http.Request, key, value interface{}) {
	*req = *(req.WithContext(context.WithValue(req.Context(), key, value)))
}

// GetSecurityContext tries to get security context from request and returns it
func GetSecurityContext(req *http.Request) (security.Context, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}

	ctx := req.Context().Value(SecurCtxKey)
	if ctx == nil {
		return nil, fmt.Errorf("the security context got from request is nil")
	}

	c, ok := ctx.(security.Context)
	if !ok {
		return nil, fmt.Errorf("the variable got from request is not security context type")
	}

	return c, nil
}

// GetProjectManager tries to get project manager from request and returns it
func GetProjectManager(req *http.Request) (promgr.ProjectManager, error) {
	if req == nil {
		return nil, fmt.Errorf("request is nil")
	}

	pm := req.Context().Value(PmKey)
	if pm == nil {
		return nil, fmt.Errorf("the project manager got from request is nil")
	}

	p, ok := pm.(promgr.ProjectManager)
	if !ok {
		return nil, fmt.Errorf("the variable got from request is not project manager type")
	}

	return p, nil
}
