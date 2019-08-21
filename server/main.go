package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/go-pg/pg/v9"
	"github.com/joeshaw/envdecode"
	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Main
//
//
//
//////////////////////////////////////////////////////////////////////////////

func main() {
	err := envdecode.Decode(&conf)
	if err != nil {
		log.Fatal(errors.Wrap(err, "error reading configuration"))
	}

	opts, err := pg.ParseURL(conf.DatabaseURL)
	if err != nil {
		log.Fatal(errors.Wrap(err, "error parsing connection string"))
	}

	db = pg.Connect(opts)
	defer db.Close()

	router := httprouter.New()
	router.PUT("/zones/:zone/records/:record",
		handlerWrapper(putRecord, &putRecordParams{}))

	log.Infof("Starting server on %v", conf.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%v", conf.Port), router))
}

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Handlers
//
//
//
//////////////////////////////////////////////////////////////////////////////

type putRecordParams struct {
	RecordType RecordType `json:"type"`
	Value      string     `json:"value"`
}

// Empty creates a new, empty version of parameters that can be decoded to.
func (*putRecordParams) Empty() BodyParams {
	return &putRecordParams{}
}

type putRecordResponse struct {
	Name       string     `json:"name"`
	RecordType RecordType `json:"type"`
	Value      string     `json:"value"`
}

func putRecord(w http.ResponseWriter, r *http.Request, state *RequestState) (interface{}, error) {
	zoneName := state.RouteParams.ByName("zone")
	recordName := state.RouteParams.ByName("record")
	bodyParams := state.BodyParams.(*putRecordParams)

	err := state.DB.RunInTransaction(func(tx *pg.Tx) error {
		var cloudflareZoneID string
		{
			var res cloudflareGetZonesResponse
			err := makeCloudflareAPICall(http.MethodGet, "/zones?name="+zoneName,
				nil, &res)
			if err != nil {
				return errors.Wrap(err, "error retrieving Coudflare zones")
			}

			if len(res.Result) > 0 {
				cloudflareZoneID = res.Result[0].ID
			}
		}

		if cloudflareZoneID == "" {
			err := makeCloudflareAPICall(http.MethodPost, "/zones",
				&cloudflareCreateZoneRequest{Name: zoneName}, nil)
			if err != nil {
				return errors.Wrap(err, "error creating Coudflare zone")
			}
		}

		var zone *Zone
		err := maybePreemptiveCancelDB(state, func() error {
			zone = &Zone{
				Name: zoneName,
			}

			_, err := state.DB.Model(zone).
				OnConflict("(name) DO UPDATE").
				Set("updated_at = NOW()").
				Returning("*").
				Insert()
			if err != nil {
				return errors.Wrap(err, "error upserting zone")
			}

			return nil
		})
		if err != nil {
			return err
		}

		var cloudflareRecordID string
		{
			getRecordsPath := "/zones/" + cloudflareZoneID + "/dns_records?name=" + recordName
			var res cloudflareGetRecordsResponse
			err := makeCloudflareAPICall(http.MethodGet, getRecordsPath,
				nil, &res)
			if err != nil {
				return errors.Wrap(err, "error retrieving Cloudflare record")
			}

			if len(res.Result) > 0 {
				cloudflareRecordID = res.Result[0].ID
			}
		}

		{
			upsertRecordMethod := http.MethodPost
			upsertRecordPath := "/zones/" + cloudflareZoneID + "/dns_records"
			if cloudflareRecordID != "" {
				upsertRecordMethod = http.MethodPut
				upsertRecordPath = "/zones/" + cloudflareZoneID + "/dns_records/" + cloudflareRecordID
			}

			err := makeCloudflareAPICall(upsertRecordMethod, upsertRecordPath,
				&cloudflareCreateRecordRequest{
					Content: bodyParams.Value,
					Name:    recordName,
					Type:    bodyParams.RecordType,
				},
				nil)
			if err != nil {
				return errors.Wrap(err, "error creating or updating Coudflare record")
			}
		}

		err = maybePreemptiveCancelDB(state, func() error {
			record := &Record{
				Name:       recordName,
				RecordType: bodyParams.RecordType,
				Value:      bodyParams.Value,
				ZoneID:     zone.ID,
			}

			_, err := state.DB.Model(record).
				OnConflict("(name, record_type, zone_id) DO UPDATE").
				Set("updated_at = NOW(), value = EXCLUDED.value").
				Returning("*").
				Insert()
			if err != nil {
				return errors.Wrap(err, "error upserting record")
			}

			return nil
		})
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "error in transaction")
	}

	return &putRecordResponse{
		Name:       recordName,
		RecordType: bodyParams.RecordType,
		Value:      bodyParams.Value,
	}, nil
}

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Constants
//
//
//
//////////////////////////////////////////////////////////////////////////////

// Constants for common record types.
const (
	RecordTypeA     RecordType = "A"
	RecordTypeAAAA  RecordType = "AAAA"
	RecordTypeCNAME RecordType = "CNAME"
)

const (
	// The amount of time to allow for an HTTP API call before it times out and
	// cancels.
	httpTimeout = 3 * time.Second

	// Maximum body size (in bytes) to protect against endless streams sent via
	// request body.
	maxRequestBodySize = 64 * 1024
)

// Preemptive cancellation thresholds for various types of operations. If
// we're within this much time of a request's deadline and we're about to
// engage in something we know to be relatively expensive, cancel the
// request instead. This allows us to avoid doing anymore work for a
// request that's unlikely to succeed.
//
// The specific thresholds for each operation depend on how long we can
// generally expect it to take. We can expect calling out to an external
// service across a network to be more expensive than a database call for
// example, so the former has a higher threshold, meaning that we'll cancel
// preemptively more aggressively.
const (
	preemptiveCancelThresholdDB           = 5 * time.Millisecond
	preemptiveCancelThresholdHandlerStart = 2 * time.Second
)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Variables
//
//
//
//////////////////////////////////////////////////////////////////////////////

// Server configuration read in from environmental variables.
var conf Conf

// Global database connection pool initialized by the core of the program. API
// handlers should never use this, and exclusively use the context-based
// version passed to them as part of `RequestState`.
var db *pg.DB

// Common API errors for consistency and quick access.
var (
	APIErrorBodyDecode       = &APIError{http.StatusBadRequest, "Error parsing request body to JSON", nil}
	APIErrorBodyEmpty        = &APIError{http.StatusBadRequest, "Request body was empty", nil}
	APIErrorBodyMax          = &APIError{http.StatusBadRequest, "Request body was too long", nil}
	APIErrorBodyRead         = &APIError{http.StatusBadRequest, "Error reading request body", nil}
	APIErrorPreemptiveCancel = &APIError{http.StatusServiceUnavailable, "Request timed out (cancelled preemptively)", nil}
	APIErrorInternal         = &APIError{http.StatusInternalServerError, "Internal server error", nil}
	APIErrorTimeout          = &APIError{http.StatusServiceUnavailable, "Request timed out", nil}
)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Types
//
//
//
//////////////////////////////////////////////////////////////////////////////

// APIError represents an error to return from the API.
type APIError struct {
	StatusCode int    `json:"status"`
	Message    string `json:"message"`

	// internalErr is an internal occur that occurred in the case of a 500.
	internalErr error
}

// Error returns a human-readable error string.
func (e *APIError) Error() string {
	return fmt.Sprintf("API error status %v: %s", e.StatusCode, e.Message)
}

// MarshalJSON provides a custom JSON encoding implementation for APIError.
//
// It works almost the same as standard encoding would except that in the case
// of a non-500 error that's carrying an internal error, we include the cause
// line of that internal error, which gives the user a little more context on
// what went wrong. So for example if we had a JSON decoding error, we'd print
// the specific error that Go produced along with our generic message about a
// body decoding problem.
func (e *APIError) MarshalJSON() ([]byte, error) {
	type apiError APIError

	dupErr := &apiError{
		StatusCode: e.StatusCode,
		Message:    e.Message,
	}

	if e.internalErr != nil && e.StatusCode != http.StatusInternalServerError {
		// `errors.Cause` just returns the inner error instead of the entirety
		// of the context
		dupErr.Message += " (" + errors.Cause(e.internalErr).Error() + ")"
	}

	return json.Marshal(dupErr)

}

// WithInternalError duplicates the given APIError and adds the given internal
// error as additional context. This is most useful for adding additional
// information to a predefined API error without mutating the original.
func (e *APIError) WithInternalError(internalErr error) *APIError {
	return &APIError{e.StatusCode, e.Message, internalErr}
}

// BodyParams is an interface containing parameters sent to an API method as
// part of a request body.
type BodyParams interface {
	// Empty creates a new, empty version of parameters that can be decoded to.
	Empty() BodyParams
}

// Conf is server configuration read in from environmental variables.
type Conf struct {
	CloudflareEmail string `env:"CLOUDFLARE_EMAIL,required"`
	CloudflareToken string `env:"CLOUDFLARE_TOKEN,required"`
	DatabaseURL     string `env:"DATABASE_URL,required"`
	Port            uint16 `env:"PORT,default=8788"`
}

// Record represents a single DNS record within a zone.
type Record struct {
	ID         int64
	CreatedAt  time.Time
	Name       string
	RecordType RecordType
	UpdatedAt  time.Time
	Value      string
	ZoneID     int64

	tableName struct{} `sql:"record"`
}

// RecordType is the type of a DNS record (e.g. A, CNAME).
type RecordType string

// RequestInfo stores information about the request for logging purposes.
type RequestInfo struct {
	APIError   *APIError
	Start      time.Time
	StatusCode int
	TimeLeft   time.Duration
	TimedOut   bool
}

// RequestState contains key data for an active request.
type RequestState struct {
	BodyParams  BodyParams
	Ctx         context.Context
	CtxCancel   func()
	DB          *pg.DB
	RequestInfo *RequestInfo
	RouteParams httprouter.Params
}

// Zone represents a logical grouping of DNS records around a particular
// domain.
type Zone struct {
	ID        int64
	CreatedAt time.Time
	Name      string
	UpdatedAt time.Time

	tableName struct{} `sql:"zone"`
}

// handler is the internal signature for an HTTP handler which includes a
// RequestState.
//
// Handlers should return either an object that should be encoded to JSON for a
// 200 response (emitted as `interface{}`) or an error. The caller should also
// encode an error response to JSON.
type handler func(w http.ResponseWriter, r *http.Request, state *RequestState) (interface{}, error)

//////////////////////////////////////////////////////////////////////////////
//
//
//
// Helper functions
//
//
//
//////////////////////////////////////////////////////////////////////////////

func handlerWrapper(handler handler, bodyParams BodyParams) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, routeParams httprouter.Params) {
		ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
		defer cancel()

		requestInfo := &RequestInfo{
			Start: time.Now(),
		}
		defer func() {
			deadline, ok := ctx.Deadline()
			requestInfo.TimeLeft = deadline.Sub(time.Now())
			requestInfo.TimedOut = !ok

			log.WithFields(log.Fields{
				"api_error": requestInfo.APIError,
				"duration":  time.Now().Sub(requestInfo.Start),
				"status":    requestInfo.StatusCode,
				"time_left": requestInfo.TimeLeft,
				"timed_out": requestInfo.TimedOut,
			}).Info("canonical_log_line")
		}()

		ctxDB := db.WithContext(ctx)

		state := &RequestState{
			Ctx:         ctx,
			CtxCancel:   cancel,
			DB:          ctxDB,
			RequestInfo: requestInfo,
			RouteParams: routeParams,
		}

		if bodyParams != nil {
			// To protect against a malicious request with an unbounded request
			// body (which could overflow memory), make sure to institute a maximum
			// length that we're willing to read.
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

			data, err := ioutil.ReadAll(r.Body)
			if err != nil {
				renderError(w, requestInfo,
					APIErrorBodyRead.WithInternalError(err))
				return
			}
			r.Body.Close()

			if len(data) == 0 {
				renderError(w, requestInfo, APIErrorBodyEmpty)
				return
			}

			// If we actually hit maximum size then it's likely that the JSON
			// decoding is going to fail with a confusing error so send a
			// specialized message back.
			if len(data) == maxRequestBodySize {
				renderError(w, requestInfo, APIErrorBodyMax)
				return
			}

			bodyParamsDup := bodyParams.Empty()
			err = json.Unmarshal(data, bodyParamsDup)
			if err != nil {
				renderError(w, requestInfo,
					APIErrorBodyDecode.WithInternalError(err))
				return
			}

			state.BodyParams = bodyParamsDup
		}

		// If we're within a certain deadline threshold before even starting
		// the handler then cancel preemptively. This could occur, for
		// example, if the client's connection is very slow and it took them a
		// long time to stream their request body to us.
		if shouldPreemptiveCancel(state.Ctx, preemptiveCancelThresholdHandlerStart) {
			state.CtxCancel()
			renderError(w, requestInfo, APIErrorPreemptiveCancel)
			return
		}

		resp, err := handler(w, r, state)
		if err != nil {
			renderError(w, requestInfo,
				errors.Wrap(err, "error serving HTTP request"))
			return
		}

		respData, err := json.Marshal(resp)
		if err != nil {
			renderError(w, requestInfo,
				errors.Wrap(err, "error encoding response"))
			return
		}

		requestInfo.StatusCode = http.StatusOK

		w.WriteHeader(http.StatusOK)
		w.Write(respData)
	}
}

const (
	cloudflareAPIURL = "https://api.cloudflare.com/client/v4"
)

// Single Cloudflare API error that may be returned as part of a response.
type cloudflareErrorItem struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cloudflareCreateRecordRequest struct {
	Content string     `json:"content"`
	Name    string     `json:"name"`
	Type    RecordType `json:"type"`
}

type cloudflareCreateZoneRequest struct {
	Name string `json:"name"`
}

// Generic Cloudflare API response that may include a set of errors.
type cloudflareGenericResponse struct {
	Errors  []*cloudflareErrorItem `json:"errors"`
	Success bool                   `json:"success"`
}

type cloudflareGetRecordsResponse struct {
	Result []*cloudflareGetRecordsResult `json:"result"`
}

type cloudflareGetRecordsResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cloudflareGetZonesResponse struct {
	Result []*cloudflareGetZonesResult `json:"result"`
}

type cloudflareGetZonesResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func flattenCloudflareErrorItems(errors []*cloudflareErrorItem) string {
	errorStrings := make([]string, len(errors))
	for i, item := range errors {
		errorStrings[i] = fmt.Sprintf("Code %v: %s", item.Code, item.Message)
	}
	return strings.Join(errorStrings, "; ")
}

func makeCloudflareAPICall(method, path string, params interface{}, res interface{}) error {
	client := http.Client{}

	log.Debugf("Making Cloudflare API request: %s %s", method, path)

	// Maybe send API parameters, but not every API call needs them.
	var reader io.Reader
	if params != nil {
		data, err := json.Marshal(params)
		if err != nil {
			return errors.Wrap(err, "error encoding Cloudflare API parameters")
		}

		log.Infof("Payload data: %s", string(data))

		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, cloudflareAPIURL+path, reader)
	if err != nil {
		return errors.Wrap(err, "error creating Cloudflare API request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Email", conf.CloudflareEmail)
	req.Header.Set("X-Auth-Key", conf.CloudflareToken)

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "error making Cloudflare API request")
	}

	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "error reading Cloudflare API response body")
	}

	// Do a very basic decoding of response to see whether there are any
	// errors.
	var genericRes cloudflareGenericResponse
	err = json.Unmarshal(data, &genericRes)
	if err != nil {
		return errors.Wrap(err, "error decoding Cloudflare API response (generic)")
	}

	if len(genericRes.Errors) > 0 {
		return &APIError{
			StatusCode: http.StatusBadRequest,
			Message: fmt.Sprintf("Errors from CloudFlare: %+v",
				flattenCloudflareErrorItems(genericRes.Errors)),
		}
	}

	// Then do the full decoding with the value sent by user (if they requested
	// it).
	if res != nil {
		err = json.Unmarshal(data, res)
		if err != nil {
			return errors.Wrap(err, "error decoding Cloudflare API response")
		}
	}

	return nil
}

// Runs a database call unless the request has taken a long time and we're too
// close to the preemptive cancellation threshold.
func maybePreemptiveCancelDB(state *RequestState, f func() error) error {
	if shouldPreemptiveCancel(state.Ctx, preemptiveCancelThresholdDB) {
		state.CtxCancel()
		return APIErrorPreemptiveCancel
	}

	return f()
}

func renderError(w http.ResponseWriter, info *RequestInfo, err error) {
	// `errors.Cause` unwraps an original error that might be wrapped up in
	// some context from the `errors` package. It's key to call it for
	// comparison purposes (or to do type checks).
	causeErr := errors.Cause(err)

	// Some special cases for common error that may occur inwards from our
	// stack which we want to convert to something more user-friendly.
	switch causeErr {
	case context.DeadlineExceeded:
		err = APIErrorTimeout.WithInternalError(err)
	}

	apiErr, ok := causeErr.(*APIError)

	// Wrap a non-API error in an API error, keeping the internal error
	// intact
	if !ok {
		apiErr = APIErrorInternal.WithInternalError(err)
	}

	if apiErr.internalErr != nil {
		// Note the `%+v` to get error *and* the backtrace
		log.Errorf("Internal error while serving request: %+v",
			apiErr.internalErr)
	}

	info.APIError = apiErr
	info.StatusCode = apiErr.StatusCode

	data, err := json.Marshal(apiErr)
	if err != nil {
		log.Errorf("Error encoding API error (very bad): %v", err)

		// Fall back to just sending back the string message. This is bad when
		// the client is expecting JSON, but it should never happen.
		data = []byte(apiErr.Message)
	}

	w.WriteHeader(apiErr.StatusCode)
	w.Write(data)
}

func shouldPreemptiveCancel(ctx context.Context, threshold time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}

	return time.Now().After(deadline.Add(-threshold))
}
