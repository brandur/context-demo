package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/go-pg/pg/v9"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	//"github.com/go-pg/pg/v9/orm"
	"github.com/julienschmidt/httprouter"
)

//
// Main
//

func main() {
	opts, err := pg.ParseURL("postgres://brandur@localhost:5432/context-demo?sslmode=disable")
	if err != nil {
		panic("Couldn't parse connection string")
	}

	db = pg.Connect(opts)
	defer db.Close()

	router := httprouter.New()
	router.PUT("/zones/:zone/records/:record", handlerWrapper(putRecord))

	log.Fatal(http.ListenAndServe(":8788", router))
}

//
// Handlers
//

// handler is the internal signature for an HTTP handler which includes a
// RequestState.
//
// Handlers should return either an object that should be encoded to JSON for a
// 200 response (emitted as `interface{}`) or an error. The caller should also
// encode an error response to JSON.
type handler func(w http.ResponseWriter, r *http.Request, state *RequestState) (interface{}, error)

type putRecordParams struct {
	RecordType RecordType `json:"type"`
	Value      string     `json:"value"`
}

type putRecordResponse struct {
	Name       string     `json:"name"`
	RecordType RecordType `json:"type"`
	Value      string     `json:"value"`
}

func putRecord(w http.ResponseWriter, r *http.Request, state *RequestState) (interface{}, error) {
	zoneName := state.RouteParams.ByName("zone")
	recordName := state.RouteParams.ByName("record")

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	defer r.Body.Close()

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, APIErrorBodyRead.WithInternalError(err)
	}

	if len(data) == 0 {
		return nil, APIErrorBodyEmpty
	}

	var params putRecordParams
	err = json.Unmarshal(data, &params)
	if err != nil {
		return nil, APIErrorBodyDecode.WithInternalError(err)
	}

	err = state.DB.RunInTransaction(func(tx *pg.Tx) error {
		var zone *Zone
		err := maybeEarlyCancelDB(state.Ctx, func() error {
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

		err = maybeEarlyCancelDB(state.Ctx, func() error {
			record := &Record{
				Name:       recordName,
				RecordType: params.RecordType,
				Value:      params.Value,
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
		RecordType: params.RecordType,
		Value:      params.Value,
	}, nil
}

//
// Helpers
//

var db *pg.DB

// Common API errors for consistency and quick access.
var (
	APIErrorBodyDecode  = &APIError{http.StatusBadRequest, "Error parsing request body to JSON", nil}
	APIErrorBodyEmpty   = &APIError{http.StatusBadRequest, "Empty request body", nil}
	APIErrorBodyRead    = &APIError{http.StatusBadRequest, "Error reading request body", nil}
	APIErrorEarlyCancel = &APIError{http.StatusServiceUnavailable, "Request timed out", nil}
	APIErrorInternal    = &APIError{http.StatusInternalServerError, "Internal server error", nil}
	APIErrorTimeout     = &APIError{http.StatusServiceUnavailable, "Request timed out", nil}
)

const httpTimeout = 2 * time.Second

// Maximum body size (in bytes) to protect against endless streams sent via
// request body.
const maxRequestBodySize = 64 * 1024

const (
	earlyCancelThresholdDB = 5 * time.Millisecond
)

// Constants for common record types.
const (
	RecordTypeCNAME RecordType = "CNAME"
)

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
	StatusCode int
	TimeLeft   time.Duration
	TimedOut   bool
}

// RequestState contains key data for an active request.
type RequestState struct {
	Ctx         context.Context
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

func handlerWrapper(handler handler) httprouter.Handle {
	return func(w http.ResponseWriter, r *http.Request, routeParams httprouter.Params) {
		ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
		defer cancel()

		requestInfo := &RequestInfo{}
		defer func() {
			deadline, ok := ctx.Deadline()
			requestInfo.TimeLeft = deadline.Sub(time.Now())
			requestInfo.TimedOut = !ok

			log.WithFields(log.Fields{
				"api_error": requestInfo.APIError,
				"status":    requestInfo.StatusCode,
				"time_left": requestInfo.TimeLeft,
				"timed_out": requestInfo.TimedOut,
			}).Info("canonical_log_line")
		}()

		ctxDB := db.WithContext(ctx)

		state := &RequestState{
			Ctx:         ctx,
			DB:          ctxDB,
			RequestInfo: requestInfo,
			RouteParams: routeParams,
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

// Runs a database call unless the request has taken a long time and we're too
// close to the early cancellation threshold.
func maybeEarlyCancelDB(ctx context.Context, f func() error) error {
	if shouldEarlyCancel(ctx, earlyCancelThresholdDB) {
		// TODO: ctx.Cancel()
		return APIErrorEarlyCancel
	}

	return f()
}

func renderError(w http.ResponseWriter, info *RequestInfo, err error) {
	// Some special cases for common error that may occur inwards from our
	// stack which we want to convert to something more user-friendly.
	//
	// `errors.Cause` unwraps an original error that might be wrapped up in
	// some context from the `errors` package. It's key to call it for
	// comparison purposes.
	switch errors.Cause(err) {
	case context.DeadlineExceeded:
		err = APIErrorTimeout.WithInternalError(err)
	}

	apiErr, ok := err.(*APIError)

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

func shouldEarlyCancel(ctx context.Context, threshold time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}

	return time.Now().After(deadline.Add(-threshold))
}
