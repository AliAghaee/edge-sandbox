/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"

	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"

	"github.com/trustbloc/edge-sandbox/pkg/internal/common/support"
	"github.com/trustbloc/edge-sandbox/pkg/token"
)

const (
	login    = "/login"
	callback = "/callback"
)

// Handler http handler for each controller API endpoint
type Handler interface {
	Path() string
	Method() string
	Handle() http.HandlerFunc
}

// Operation defines handlers for authorization service
type Operation struct {
	handlers      []Handler
	tokenIssuer   tokenIssuer
	tokenResolver tokenResolver
	cmsURL        string
	vcsURL        string
	vcsProfile    string
	receiveVCHTML string
}

// Config defines configuration for issuer operations
type Config struct {
	TokenIssuer   tokenIssuer
	TokenResolver tokenResolver
	CMSURL        string
	VCSURL        string
	VCSProfile    string
	ReceiveVCHTML string
}

// vc struct used to return vc data to html
type vc struct {
	Data string `json:"data"`
}

type tokenIssuer interface {
	AuthCodeURL(w http.ResponseWriter) string
	Exchange(r *http.Request) (*oauth2.Token, error)
	Client(ctx context.Context, t *oauth2.Token) *http.Client
}

type tokenResolver interface {
	Resolve(token string) (*token.Introspection, error)
}

// New returns authorization instance
func New(config *Config) *Operation {
	svc := &Operation{
		tokenIssuer:   config.TokenIssuer,
		tokenResolver: config.TokenResolver,
		cmsURL:        config.CMSURL,
		vcsURL:        config.VCSURL,
		vcsProfile:    config.VCSProfile,
		receiveVCHTML: config.ReceiveVCHTML}
	svc.registerHandler()

	return svc
}

// Login using oauth2, will redirect to Auth Code URL
func (c *Operation) Login(w http.ResponseWriter, r *http.Request) {
	u := c.tokenIssuer.AuthCodeURL(w)
	http.Redirect(w, r, u, http.StatusTemporaryRedirect)
}

// Callback for oauth2 login
func (c *Operation) Callback(w http.ResponseWriter, r *http.Request) {
	tk, err := c.tokenIssuer.Exchange(r)
	if err != nil {
		log.Error(err)
		c.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to exchange code for token: %s", err.Error()))

		return
	}

	// user info from token will be used for to retrieve data from cms
	_, err = c.tokenResolver.Resolve(tk.AccessToken)
	if err != nil {
		log.Error(err)
		c.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get token info: %s", err.Error()))

		return
	}

	data, err := c.getCMSData(tk)
	if err != nil {
		log.Error(err)
		c.writeErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("failed to get user cms data: %s", err.Error()))

		return
	}

	cred, err := c.createCredential(data)
	if err != nil {
		log.Error(err)
		c.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to create credential: %s", err.Error()))

		return
	}

	err = c.storeCredential(cred)
	if err != nil {
		log.Error(err)
		c.writeErrorResponse(w, http.StatusInternalServerError,
			fmt.Sprintf("failed to store credential: %s", err.Error()))

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	t, err := template.ParseFiles(c.receiveVCHTML)
	if err != nil {
		log.Error(err)
		c.writeErrorResponse(w, http.StatusInternalServerError, fmt.Sprintf("unable to load html: %s", err.Error()))

		return
	}

	if err := t.Execute(w, vc{Data: string(cred)}); err != nil {
		log.Error(fmt.Sprintf("failed execute html template: %s", err.Error()))
	}
}

func (c *Operation) prepareCreateCredentialRequest(data []byte) ([]byte, error) {
	var subject map[string]interface{}

	err := json.Unmarshal(data, &subject)
	if err != nil {
		return nil, err
	}

	// remove cms id, add name as id (will be replaced by DID)
	subject["id"] = subject["name"]

	// remove cms specific fields
	delete(subject, "created_at")
	delete(subject, "updated_at")

	req := &createCredential{
		Subject: subject,
		Type:    []string{"VerifiableCredential", "StudentCard"},
		Profile: c.vcsProfile,
	}

	return json.Marshal(req)
}

func (c *Operation) createCredential(subject []byte) ([]byte, error) {
	body, err := c.prepareCreateCredentialRequest(subject)

	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", c.vcsURL+"/credential", bytes.NewBuffer(body))

	if err != nil {
		return nil, err
	}

	httpClient := http.DefaultClient

	return sendHTTPRequest(req, httpClient, http.StatusCreated)
}

func (c *Operation) storeCredential(cred []byte) error {
	storeVCBytes, err := prepareStoreVCRequest(cred, c.vcsProfile)
	if err != nil {
		return err
	}

	storeReq, err := http.NewRequest("POST", c.vcsURL+"/store", bytes.NewBuffer(storeVCBytes))

	if err != nil {
		return err
	}

	httpClient := http.DefaultClient

	_, err = sendHTTPRequest(storeReq, httpClient, http.StatusOK)
	if err != nil {
		return err
	}

	return nil
}
func prepareStoreVCRequest(cred []byte, profile string) ([]byte, error) {
	storeVCRequest := storeVC{
		Credential: string(cred),
		Profile:    profile,
	}

	return json.Marshal(storeVCRequest)
}

func (c *Operation) getCMSData(tk *oauth2.Token) ([]byte, error) {
	httpClient := c.tokenIssuer.Client(context.Background(), tk)

	req, err := http.NewRequest("GET", c.cmsURL+"/studentcards/1", nil)
	if err != nil {
		return nil, err
	}

	return sendHTTPRequest(req, httpClient, http.StatusOK)
}

func sendHTTPRequest(req *http.Request, client *http.Client, status int) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		err = resp.Body.Close()
		if err != nil {
			log.Warn("failed to close response body")
		}
	}()

	if resp.StatusCode != status {
		return nil, errors.New(resp.Status)
	}

	return ioutil.ReadAll(resp.Body)
}

// registerHandler register handlers to be exposed from this service as REST API endpoints
func (c *Operation) registerHandler() {
	// Add more protocol endpoints here to expose them as controller API endpoints
	c.handlers = []Handler{
		support.NewHTTPHandler(login, http.MethodGet, c.Login),
		support.NewHTTPHandler(callback, http.MethodGet, c.Callback),
	}
}

// writeResponse writes interface value to response
func (c *Operation) writeErrorResponse(rw http.ResponseWriter, status int, msg string) {
	rw.WriteHeader(status)

	if _, err := rw.Write([]byte(msg)); err != nil {
		log.Errorf("Unable to send error message, %s", err)
	}
}

// GetRESTHandlers get all controller API handler available for this service
func (c *Operation) GetRESTHandlers() []Handler {
	return c.handlers
}

// createCredential input data for edge service issuer rest api
type createCredential struct {
	Subject map[string]interface{} `json:"credentialSubject"`
	Type    []string               `json:"type,omitempty"`
	Profile string                 `json:"profile,omitempty"`
}

type storeVC struct {
	Credential string `json:"credential"`
	Profile    string `json:"profile,omitempty"`
}
