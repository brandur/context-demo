package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-pg/pg/v9"
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
	router.PUT("/zones/:zone/records/:record", putRecord)

	log.Fatal(http.ListenAndServe(":8080", router))
}

//
// Handlers
//

func putRecord(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()

	info := &RequestInfo{}
	defer func() {
		deadline, ok := ctx.Deadline()
		info.TimeLeft = deadline.Sub(time.Now())
		info.TimedOut = !ok

		log.WithFields(log.Fields{
			"time_left": info.TimeLeft,
			"timed_out": info.TimedOut,
		}).Info("canonical_log_line")
	}()

	ctxDB := db.WithContext(ctx)

	zoneName := ps.ByName("zone")
	recordName := ps.ByName("record")

	err := ctxDB.RunInTransaction(func(tx *pg.Tx) error {
		if shouldEarlyCancel(ctx, earlyCancelThresholdDB) {
			return fmt.Errorf("Early cancel")
		}

		var zone *Zone
		{
			zone = &Zone{
				Name: zoneName,
			}

			_, err := db.Model(zone).
				OnConflict("(name) DO UPDATE").
				Set("updated_at = NOW()").
				Returning("*").
				Insert()
			if err != nil {
				return err
			}
		}

		record := &Record{
			Name:       recordName,
			RecordType: RecordTypeCNAME,
			ZoneID:     zone.ID,
		}

		{
			_, err := db.Model(record).
				OnConflict("(name, record_type, zone_id) DO UPDATE").
				Set("updated_at = NOW()").
				Returning("*").
				Insert()
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		render500(w, err)
		return
	}

	fmt.Fprintf(w, "zone: %s, cname: %s\n", zoneName, recordName)
}

//
// Helpers
//

var db *pg.DB

const httpTimeout = 10 * time.Second

const (
	earlyCancelThresholdDB = 5 * time.Millisecond
)

// Constants for common record types.
const (
	RecordTypeCNAME RecordType = "CNAME"
)

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
	TimeLeft time.Duration
	TimedOut bool
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

func render500(w http.ResponseWriter, err error) {
	log.Errorf("Error while serving request: %v", err)

	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, "Internal server error")
}

func shouldEarlyCancel(ctx context.Context, threshold time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}

	return time.Now().After(deadline.Add(-threshold))
}
