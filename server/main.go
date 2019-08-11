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
type handler func(w http.ResponseWriter, r *http.Request, state *RequestState) error

type putRecordParams struct {
	RecordType RecordType `json:"type"`
}

func putRecord(w http.ResponseWriter, r *http.Request, state *RequestState) error {
	zoneName := state.RouteParams.ByName("zone")
	recordName := state.RouteParams.ByName("record")

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	defer r.Body.Close()

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return &APIError{
			StatusCode: http.StatusBadRequest,
			Message:    "Error reading request body",
		}
	}

	if len(data) == 0 {
		return &APIError{
			StatusCode: http.StatusBadRequest,
			Message:    "Empty request body",
		}
	}

	var params putRecordParams
	err = json.Unmarshal(data, &params)
	if err != nil {
		return &APIError{
			StatusCode: http.StatusBadRequest,
			Message:    "Error parsing request body to JSON",
		}
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
				RecordType: RecordTypeCNAME,
				ZoneID:     zone.ID,
			}

			_, err := state.DB.Model(record).
				OnConflict("(name, record_type, zone_id) DO UPDATE").
				Set("updated_at = NOW()").
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
		return errors.Wrap(err, "error in transaction")
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "zone: %s, cname: %s\n", zoneName, recordName)

	return nil
}

//
// Helpers
//

var db *pg.DB

// Common API errors for consistency and quick access.
var (
	APIErrorEarlyCancel = &APIError{StatusCode: http.StatusServiceUnavailable, Message: "Request timed out"}
	APIErrorTimeout     = &APIError{StatusCode: http.StatusServiceUnavailable, Message: "Request timed out"}
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
	StatusCode int
	Message    string

	// internalErr is an internal occur that occurred in the case of a 500.
	internalErr error
}

// Error returns a human-readable error string.
func (e *APIError) Error() string {
	return fmt.Sprintf("API error status %v: %s", e.StatusCode, e.Message)
}

// Record represents a single DNS record within a zone.
type Record struct {
	ID         int64
	CreatedAt  time.Time
	Name       string
	RecordType RecordType
	UpdatedAt  time.Time
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

		err := handler(w, r, state)
		if err != nil {
			renderError(w, requestInfo,
				errors.Wrap(err, "error serving HTTP request"))
			return
		}

		requestInfo.StatusCode = http.StatusOK
	}
}

// Runs a database call unless the request has taken a long time and we're too
// close to the early cancellation threshold.
func maybeEarlyCancelDB(ctx context.Context, f func() error) error {
	if shouldEarlyCancel(ctx, earlyCancelThresholdDB) {
		return APIErrorEarlyCancel
	}

	return f()
}

func renderError(w http.ResponseWriter, info *RequestInfo, err error) {
	apiErr, ok := err.(*APIError)

	// Wrap a non-API error in an API error, keeping the internal error
	// intact
	if !ok {
		apiErr = &APIError{
			StatusCode:  http.StatusInternalServerError,
			Message:     "Internal server error",
			internalErr: err,
		}
	}

	if apiErr.internalErr != nil {
		// Note the `%+v` to get error *and* the backtrace
		log.Errorf("Internal error while serving request: %+v",
			apiErr.internalErr)
	}

	info.APIError = apiErr
	info.StatusCode = apiErr.StatusCode

	w.WriteHeader(apiErr.StatusCode)
	fmt.Fprintf(w, apiErr.Message)
}

func shouldEarlyCancel(ctx context.Context, threshold time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}

	return time.Now().After(deadline.Add(-threshold))
}
