package idp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/crewjam/saml"
	"github.com/gorilla/mux"
	"github.com/muhlemmer/gu"
	"github.com/zitadel/logging"

	"github.com/zitadel/zitadel/internal/api/authz"
	http_utils "github.com/zitadel/zitadel/internal/api/http"
	"github.com/zitadel/zitadel/internal/api/ui/login"
	"github.com/zitadel/zitadel/internal/cache"
	"github.com/zitadel/zitadel/internal/command"
	"github.com/zitadel/zitadel/internal/crypto"
	"github.com/zitadel/zitadel/internal/domain/federatedlogout"
	"github.com/zitadel/zitadel/internal/form"
	"github.com/zitadel/zitadel/internal/idp"
	"github.com/zitadel/zitadel/internal/idp/providers/apple"
	"github.com/zitadel/zitadel/internal/idp/providers/azuread"
	"github.com/zitadel/zitadel/internal/idp/providers/github"
	"github.com/zitadel/zitadel/internal/idp/providers/gitlab"
	"github.com/zitadel/zitadel/internal/idp/providers/google"
	"github.com/zitadel/zitadel/internal/idp/providers/jwt"
	"github.com/zitadel/zitadel/internal/idp/providers/ldap"
	"github.com/zitadel/zitadel/internal/idp/providers/oauth"
	openid "github.com/zitadel/zitadel/internal/idp/providers/oidc"
	saml2 "github.com/zitadel/zitadel/internal/idp/providers/saml"
	"github.com/zitadel/zitadel/internal/query"
	"github.com/zitadel/zitadel/internal/zerrors"
)

const (
	HandlerPrefix = "/idps"

	idpPrefix = "/{" + varIDPID + ":[0-9]+}"

	callbackPath    = "/callback"
	metadataPath    = idpPrefix + "/saml/metadata"
	acsPath         = idpPrefix + "/saml/acs"
	certificatePath = idpPrefix + "/saml/certificate"
	sloPath         = idpPrefix + "/saml/slo"
	jwtPath         = "/jwt"

	paramIntentID         = "id"
	paramToken            = "token"
	paramUserID           = "user"
	paramError            = "error"
	paramErrorDescription = "error_description"
	varIDPID              = "idpid"
	paramInternalUI       = "internalUI"
)

type Handler struct {
	commands            *command.Commands
	queries             *query.Queries
	parser              *form.Parser
	encryptionAlgorithm crypto.EncryptionAlgorithm
	callbackURL         func(ctx context.Context) string
	samlRootURL         func(ctx context.Context, idpID string) string
	loginSAMLRootURL    func(ctx context.Context) string
	caches              *Caches
}

type externalIDPCallbackData struct {
	State            string `schema:"state"`
	Code             string `schema:"code"`
	Error            string `schema:"error"`
	ErrorDescription string `schema:"error_description"`

	// Apple returns a user on first registration
	User string `schema:"user"`
}

type externalSAMLIDPCallbackData struct {
	IDPID      string
	Response   string
	RelayState string
}

// CallbackURL generates the instance specific URL to the IDP callback handler
func CallbackURL() func(ctx context.Context) string {
	return func(ctx context.Context) string {
		return http_utils.DomainContext(ctx).Origin() + HandlerPrefix + callbackPath
	}
}

func SAMLRootURL() func(ctx context.Context, idpID string) string {
	return func(ctx context.Context, idpID string) string {
		return http_utils.DomainContext(ctx).Origin() + HandlerPrefix + "/" + idpID + "/"
	}
}

func LoginSAMLRootURL() func(ctx context.Context) string {
	return func(ctx context.Context) string {
		return http_utils.DomainContext(ctx).Origin() + login.HandlerPrefix + login.EndpointSAMLACS
	}
}

func NewHandler(
	commands *command.Commands,
	queries *query.Queries,
	encryptionAlgorithm crypto.EncryptionAlgorithm,
	instanceInterceptor func(next http.Handler) http.Handler,
	federatedLogoutCache cache.Cache[federatedlogout.Index, string, *federatedlogout.FederatedLogout],
) http.Handler {
	h := &Handler{
		commands:            commands,
		queries:             queries,
		parser:              form.NewParser(),
		encryptionAlgorithm: encryptionAlgorithm,
		callbackURL:         CallbackURL(),
		samlRootURL:         SAMLRootURL(),
		loginSAMLRootURL:    LoginSAMLRootURL(),
		caches:              &Caches{federatedLogouts: federatedLogoutCache},
	}

	router := mux.NewRouter()
	router.Use(instanceInterceptor)
	router.HandleFunc(callbackPath, h.handleCallback)
	router.HandleFunc(metadataPath, h.handleMetadata)
	router.HandleFunc(certificatePath, h.handleCertificate)
	router.HandleFunc(acsPath, h.handleACS)
	router.HandleFunc(sloPath, h.handleSLO)
	router.HandleFunc(jwtPath, h.handleJWT)
	return router
}

type Caches struct {
	federatedLogouts cache.Cache[federatedlogout.Index, string, *federatedlogout.FederatedLogout]
}

func parseSAMLRequest(r *http.Request) *externalSAMLIDPCallbackData {
	vars := mux.Vars(r)
	return &externalSAMLIDPCallbackData{
		IDPID:      vars[varIDPID],
		Response:   r.FormValue("SAMLResponse"),
		RelayState: r.FormValue("RelayState"),
	}
}

func (h *Handler) getProvider(ctx context.Context, idpID string) (idp.Provider, error) {
	return h.commands.GetProvider(ctx, idpID, h.callbackURL(ctx), h.samlRootURL(ctx, idpID))
}

func (h *Handler) handleCertificate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := parseSAMLRequest(r)

	provider, err := h.getProvider(ctx, data.IDPID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	samlProvider, ok := provider.(*saml2.Provider)
	if !ok {
		http.Error(w, zerrors.ThrowInvalidArgument(nil, "SAML-lrud8s9coi", "Errors.Intent.IDPInvalid").Error(), http.StatusBadRequest)
		return
	}

	certPem := new(bytes.Buffer)
	if _, err := certPem.Write(samlProvider.Certificate); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=idp.crt")
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	_, err = io.Copy(w, certPem)
	if err != nil {
		http.Error(w, fmt.Errorf("failed to response with certificate: %w", err).Error(), http.StatusInternalServerError)
		return
	}
}

func (h *Handler) handleMetadata(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := parseSAMLRequest(r)

	provider, err := h.getProvider(ctx, data.IDPID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	samlProvider, ok := provider.(*saml2.Provider)
	if !ok {
		http.Error(w, zerrors.ThrowInvalidArgument(nil, "SAML-lrud8s9coi", "Errors.Intent.IDPInvalid").Error(), http.StatusBadRequest)
		return
	}

	sp, err := samlProvider.GetSP()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	metadata := sp.ServiceProvider.Metadata()

	internalUI, _ := strconv.ParseBool(r.URL.Query().Get(paramInternalUI))
	h.assertionConsumerServices(ctx, metadata, internalUI)

	buf, _ := xml.MarshalIndent(metadata, "", "  ")
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, err = w.Write(buf)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
}

func (h *Handler) assertionConsumerServices(ctx context.Context, metadata *saml.EntityDescriptor, internalUI bool) {
	if !internalUI {
		for i, spDesc := range metadata.SPSSODescriptors {
			spDesc.AssertionConsumerServices = append(
				spDesc.AssertionConsumerServices,
				saml.IndexedEndpoint{
					Binding:  saml.HTTPPostBinding,
					Location: h.loginSAMLRootURL(ctx),
					Index:    len(spDesc.AssertionConsumerServices) + 1,
				}, saml.IndexedEndpoint{
					Binding:  saml.HTTPArtifactBinding,
					Location: h.loginSAMLRootURL(ctx),
					Index:    len(spDesc.AssertionConsumerServices) + 2,
				},
			)
			metadata.SPSSODescriptors[i] = spDesc
		}
		return
	}
	for i, spDesc := range metadata.SPSSODescriptors {
		acs := make([]saml.IndexedEndpoint, 0, len(spDesc.AssertionConsumerServices)+2)
		acs = append(acs,
			saml.IndexedEndpoint{
				Binding:   saml.HTTPPostBinding,
				Location:  h.loginSAMLRootURL(ctx),
				Index:     0,
				IsDefault: gu.Ptr(true),
			},
			saml.IndexedEndpoint{
				Binding:  saml.HTTPArtifactBinding,
				Location: h.loginSAMLRootURL(ctx),
				Index:    1,
			})
		for i := 0; i < len(spDesc.AssertionConsumerServices); i++ {
			spDesc.AssertionConsumerServices[i].Index = 2 + i
			acs = append(acs, spDesc.AssertionConsumerServices[i])
		}
		spDesc.AssertionConsumerServices = acs
		metadata.SPSSODescriptors[i] = spDesc
	}
}

func (h *Handler) handleACS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := parseSAMLRequest(r)

	provider, err := h.getProvider(ctx, data.IDPID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	samlProvider, ok := provider.(*saml2.Provider)
	if !ok {
		err := zerrors.ThrowInvalidArgument(nil, "SAML-ui9wyux0hp", "Errors.Intent.IDPInvalid")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	intent, err := h.commands.GetActiveIntent(ctx, data.RelayState)
	if err != nil {
		if zerrors.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		redirectToFailureURLErr(w, r, intent, err)
		return
	}

	session, err := saml2.NewSession(samlProvider, intent.RequestID, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	idpUser, err := session.FetchUser(r.Context())
	if err != nil {
		cmdErr := h.commands.FailIDPIntent(ctx, intent, err.Error())
		logging.WithFields("intent", intent.AggregateID).OnError(cmdErr).Error("failed to push failed event on idp intent")
		redirectToFailureURLErr(w, r, intent, err)
		return
	}

	userID, err := h.checkExternalUser(ctx, intent.IDPID, idpUser.GetID())
	logging.WithFields("intent", intent.AggregateID).OnError(err).Error("could not check if idp user already exists")

	token, err := h.commands.SucceedSAMLIDPIntent(ctx, intent, idpUser, userID, session)
	if err != nil {
		redirectToFailureURLErr(w, r, intent, zerrors.ThrowInternal(err, "IDP-JdD3g", "Errors.Intent.TokenCreationFailed"))
		return
	}
	redirectToSuccessURL(w, r, intent, token, userID)
}

func (h *Handler) handleJWT(w http.ResponseWriter, r *http.Request) {
	intentID, err := h.intentIDFromJWTRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	intent, err := h.commands.GetActiveIntent(r.Context(), intentID)
	if err != nil {
		if zerrors.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		redirectToFailureURLErr(w, r, intent, err)
		return
	}
	idpConfig, err := h.getProvider(r.Context(), intent.IDPID)
	if err != nil {
		cmdErr := h.commands.FailIDPIntent(r.Context(), intent, err.Error())
		logging.WithFields("intent", intent.AggregateID).OnError(cmdErr).Error("failed to push failed event on idp intent")
		redirectToFailureURLErr(w, r, intent, err)
		return
	}
	jwtIDP, ok := idpConfig.(*jwt.Provider)
	if !ok {
		err := zerrors.ThrowInvalidArgument(nil, "IDP-JK23ed", "Errors.ExternalIDP.IDPTypeNotImplemented")
		cmdErr := h.commands.FailIDPIntent(r.Context(), intent, err.Error())
		logging.WithFields("intent", intent.AggregateID).OnError(cmdErr).Error("failed to push failed event on idp intent")
		redirectToFailureURLErr(w, r, intent, err)
		return
	}
	h.handleJWTExtraction(w, r, intent, jwtIDP)
}

func (h *Handler) intentIDFromJWTRequest(r *http.Request) (string, error) {
	// for compatibility of the old JWT provider we use the auth request id parameter to pass the intent id
	intentID := r.FormValue(jwt.QueryAuthRequestID)
	// for compatibility of the old JWT provider we use the user agent id parameter to pass the encrypted intent id
	encryptedIntentID := r.FormValue(jwt.QueryUserAgentID)
	if err := h.checkIntentID(intentID, encryptedIntentID); err != nil {
		return "", err
	}
	return intentID, nil
}

func (h *Handler) checkIntentID(intentID, encryptedIntentID string) error {
	if intentID == "" || encryptedIntentID == "" {
		return zerrors.ThrowInvalidArgument(nil, "LOGIN-adfzz", "Errors.AuthRequest.MissingParameters")
	}
	id, err := base64.RawURLEncoding.DecodeString(encryptedIntentID)
	if err != nil {
		return err
	}
	decryptedIntentID, err := h.encryptionAlgorithm.DecryptString(id, h.encryptionAlgorithm.EncryptionKeyID())
	if err != nil {
		return err
	}
	if intentID != decryptedIntentID {
		return zerrors.ThrowInvalidArgument(nil, "LOGIN-adfzz", "Errors.AuthRequest.MissingParameters")
	}
	return nil
}

func (h *Handler) handleJWTExtraction(w http.ResponseWriter, r *http.Request, intent *command.IDPIntentWriteModel, identityProvider *jwt.Provider) {
	session := jwt.NewSessionFromRequest(identityProvider, r)
	user, err := session.FetchUser(r.Context())
	if err != nil {
		cmdErr := h.commands.FailIDPIntent(r.Context(), intent, err.Error())
		logging.WithFields("intent", intent.AggregateID).OnError(cmdErr).Error("failed to push failed event on idp intent")
		redirectToFailureURLErr(w, r, intent, err)
		return
	}

	userID, err := h.checkExternalUser(r.Context(), intent.IDPID, user.GetID())
	logging.WithFields("intent", intent.AggregateID).OnError(err).Error("could not check if idp user already exists")

	token, err := h.commands.SucceedIDPIntent(r.Context(), intent, user, session, userID)
	if err != nil {
		redirectToFailureURLErr(w, r, intent, err)
		return
	}
	redirectToSuccessURL(w, r, intent, token, userID)
}

func (h *Handler) handleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := h.parseCallbackRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	intent, err := h.commands.GetActiveIntent(ctx, data.State)
	if err != nil {
		if zerrors.IsNotFound(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		redirectToFailureURLErr(w, r, intent, err)
		return
	}

	// the provider might have returned an error
	if data.Error != "" {
		cmdErr := h.commands.FailIDPIntent(ctx, intent, reason(data.Error, data.ErrorDescription))
		logging.WithFields("intent", intent.AggregateID).OnError(cmdErr).Error("failed to push failed event on idp intent")
		redirectToFailureURL(w, r, intent, data.Error, data.ErrorDescription)
		return
	}

	provider, err := h.getProvider(ctx, intent.IDPID)
	if err != nil {
		cmdErr := h.commands.FailIDPIntent(ctx, intent, err.Error())
		logging.WithFields("intent", intent.AggregateID).OnError(cmdErr).Error("failed to push failed event on idp intent")
		redirectToFailureURLErr(w, r, intent, err)
		return
	}

	idpUser, idpSession, err := h.fetchIDPUserFromCode(ctx, provider, data.Code, data.User, intent.IDPArguments)
	if err != nil {
		cmdErr := h.commands.FailIDPIntent(ctx, intent, err.Error())
		logging.WithFields("intent", intent.AggregateID).OnError(cmdErr).Error("failed to push failed event on idp intent")
		redirectToFailureURLErr(w, r, intent, err)
		return
	}
	userID, err := h.checkExternalUser(ctx, intent.IDPID, idpUser.GetID())
	logging.WithFields("intent", intent.AggregateID).OnError(err).Error("could not check if idp user already exists")

	if userID == "" {
		userID, err = h.tryMigrateExternalUser(ctx, intent.IDPID, idpUser, idpSession)
		logging.WithFields("intent", intent.AggregateID).OnError(err).Error("migration check failed")
	}

	token, err := h.commands.SucceedIDPIntent(ctx, intent, idpUser, idpSession, userID)
	if err != nil {
		redirectToFailureURLErr(w, r, intent, zerrors.ThrowInternal(err, "IDP-JdD3g", "Errors.Intent.TokenCreationFailed"))
		return
	}
	redirectToSuccessURL(w, r, intent, token, userID)
}

func (h *Handler) handleSLO(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := parseSAMLRequest(r)

	logoutState, ok := h.caches.federatedLogouts.Get(ctx, federatedlogout.IndexRequestID, federatedlogout.Key(authz.GetInstance(ctx).InstanceID(), data.RelayState))
	if !ok || logoutState.State != federatedlogout.StateRedirected {
		err := zerrors.ThrowNotFound(nil, "SAML-3uor2", "Errors.Intent.NotFound")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// For the moment we just make sure the callback matches the IDP it was started on / intended for.

	provider, err := h.getProvider(ctx, data.IDPID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, ok = provider.(*saml2.Provider); !ok {
		err := zerrors.ThrowInvalidArgument(nil, "SAML-ui9wyux0hp", "Errors.Intent.IDPInvalid")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// We could also parse and validate the response here, but for example Azure does not sign it and thus would already fail.
	// Also we can't really act on it if it fails.

	err = h.caches.federatedLogouts.Delete(ctx, federatedlogout.IndexRequestID, federatedlogout.Key(logoutState.InstanceID, logoutState.SessionID))
	logging.WithFields("instanceID", logoutState.InstanceID, "sessionID", logoutState.SessionID).OnError(err).Error("could not delete federated logout")
	http.Redirect(w, r, logoutState.PostLogoutRedirectURI, http.StatusFound)
}

func (h *Handler) tryMigrateExternalUser(ctx context.Context, idpID string, idpUser idp.User, idpSession idp.Session) (userID string, err error) {
	migration, ok := idpSession.(idp.SessionSupportsMigration)
	if !ok {
		return "", nil
	}
	previousID, err := migration.RetrievePreviousID()
	if err != nil || previousID == "" {
		return "", err
	}
	userID, err = h.checkExternalUser(ctx, idpID, previousID)
	if err != nil {
		return "", err
	}
	return userID, h.commands.MigrateUserIDP(ctx, userID, "", idpID, previousID, idpUser.GetID())
}

func (h *Handler) parseCallbackRequest(r *http.Request) (*externalIDPCallbackData, error) {
	data := new(externalIDPCallbackData)
	err := h.parser.Parse(r, data)
	if err != nil {
		return nil, err
	}
	if data.State == "" {
		return nil, zerrors.ThrowInvalidArgument(nil, "IDP-Hk38e", "Errors.Intent.StateMissing")
	}
	return data, nil
}

func redirectToSuccessURL(w http.ResponseWriter, r *http.Request, intent *command.IDPIntentWriteModel, token, userID string) {
	queries := intent.SuccessURL.Query()
	queries.Set(paramIntentID, intent.AggregateID)
	queries.Set(paramToken, token)
	if userID != "" {
		queries.Set(paramUserID, userID)
	}
	intent.SuccessURL.RawQuery = queries.Encode()
	http.Redirect(w, r, intent.SuccessURL.String(), http.StatusFound)
}

func redirectToFailureURLErr(w http.ResponseWriter, r *http.Request, i *command.IDPIntentWriteModel, err error) {
	msg := err.Error()
	var description string
	zErr := new(zerrors.ZitadelError)
	if errors.As(err, &zErr) {
		msg = zErr.GetID()
		description = zErr.GetMessage() // TODO: i18n?
	}
	redirectToFailureURL(w, r, i, msg, description)
}

func redirectToFailureURL(w http.ResponseWriter, r *http.Request, i *command.IDPIntentWriteModel, err, description string) {
	queries := i.FailureURL.Query()
	queries.Set(paramIntentID, i.AggregateID)
	queries.Set(paramError, err)
	queries.Set(paramErrorDescription, description)
	i.FailureURL.RawQuery = queries.Encode()
	http.Redirect(w, r, i.FailureURL.String(), http.StatusFound)
}

func (h *Handler) fetchIDPUserFromCode(ctx context.Context, identityProvider idp.Provider, code string, appleUser string, idpArguments map[string]any) (user idp.User, idpTokens idp.Session, err error) {
	var session idp.Session
	switch provider := identityProvider.(type) {
	case *oauth.Provider:
		session = oauth.NewSession(provider, code, idpArguments)
	case *openid.Provider:
		session = openid.NewSession(provider, code, idpArguments)
	case *azuread.Provider:
		session = azuread.NewSession(provider, code)
	case *github.Provider:
		session = oauth.NewSession(provider.Provider, code, idpArguments)
	case *gitlab.Provider:
		session = openid.NewSession(provider.Provider, code, idpArguments)
	case *google.Provider:
		session = openid.NewSession(provider.Provider, code, idpArguments)
	case *apple.Provider:
		session = apple.NewSession(provider, code, appleUser)
	case *jwt.Provider, *ldap.Provider, *saml2.Provider:
		return nil, nil, zerrors.ThrowInvalidArgument(nil, "IDP-52jmn", "Errors.ExternalIDP.IDPTypeNotImplemented")
	default:
		return nil, nil, zerrors.ThrowUnimplemented(nil, "IDP-SSDg", "Errors.ExternalIDP.IDPTypeNotImplemented")
	}

	user, err = session.FetchUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	return user, session, nil
}

func (h *Handler) checkExternalUser(ctx context.Context, idpID, externalUserID string) (userID string, err error) {
	idQuery, err := query.NewIDPUserLinkIDPIDSearchQuery(idpID)
	if err != nil {
		return "", err
	}
	externalIDQuery, err := query.NewIDPUserLinksExternalIDSearchQuery(externalUserID)
	if err != nil {
		return "", err
	}
	queries := []query.SearchQuery{
		idQuery, externalIDQuery,
	}
	links, err := h.queries.IDPUserLinks(ctx, &query.IDPUserLinksSearchQuery{Queries: queries}, nil)
	if err != nil {
		return "", err
	}
	if len(links.Links) != 1 {
		return "", nil
	}
	return links.Links[0].UserID, nil
}

func reason(err, description string) string {
	if description == "" {
		return err
	}
	return err + ": " + description
}
