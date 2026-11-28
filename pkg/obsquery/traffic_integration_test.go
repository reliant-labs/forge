//go:build integration

package obsquery_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/obsquery"
)

// Complete interior days must be counted only by the rollup; partial days
// must be counted only by raw requests, including when either bound is midnight.
func TestTrafficRollupMatchesRawAcrossBoundaryDays(t *testing.T) {
	c := startClickHouse(t)
	ctx := context.Background()
	const db = "t_traffic_boundaries"
	for _, sql := range []string{
		"CREATE DATABASE " + db,
		`CREATE TABLE ` + db + `.edge_requests (
    Timestamp DateTime64(9, 'UTC'), EnvId String, Site String, Path String,
    Status UInt16, CacheStatus String, UaClass String, SampleWeight UInt64, Bytes UInt64
) ENGINE = MergeTree ORDER BY Timestamp`,
		`INSERT INTO ` + db + `.edge_requests
SELECT toDateTime64('2026-11-01 00:00:00', 9, 'UTC') + toIntervalHour(number),
       'prod', 'web', if(number % 2 = 0, '/', '/docs'),
       if(number % 3 = 0, 404, 200), if(number % 2 = 0, 'hit', 'miss'),
       if(number % 2 = 0, 'browser', 'ai_user'), toUInt64(2), toUInt64(100)
FROM numbers(264)`,
		`CREATE TABLE ` + db + `.edge_requests_1d ENGINE = MergeTree ORDER BY Day AS
SELECT toDate(Timestamp, 'UTC') AS Day, EnvId, Site, Path, Status, CacheStatus, UaClass,
       sum(SampleWeight) AS Requests, sum(Bytes * SampleWeight) AS Bytes
FROM ` + db + `.edge_requests
GROUP BY Day, EnvId, Site, Path, Status, CacheStatus, UaClass`,
	} {
		if err := c.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	run := rumRunner(c)
	for _, fromHour := range []int{0, 12} {
		for _, toHour := range []int{0, 12} {
			t.Run(fmt.Sprintf("from_%d_to_%d", fromHour, toHour), func(t *testing.T) {
				sc := obsquery.Scope{Database: db, EnvRestricted: true, EnvIDs: []string{"prod"},
					Start: time.Date(2026, 11, 2, fromHour, 0, 0, 0, time.UTC),
					End:   time.Date(2026, 11, 10, toHour, 0, 0, 0, time.UTC)}
				for _, by := range []obsquery.TrafficBy{"", obsquery.TrafficByStatus} {
					var rawTotals []obsquery.TrafficTotal
					var rawRanks []obsquery.TrafficRow
					for _, rollup := range []bool{false, true} {
						q := obsquery.TrafficQuery{By: by}
						if rollup {
							q.Range = sc.End.Sub(sc.Start)
						}
						sts, err := q.Statements()
						if err != nil {
							t.Fatal(err)
						}
						for _, st := range sts {
							compiled, err := (obsquery.SQLPanel{Scope: sc, SQL: st.SQL, StepSeconds: 3600}).Compile()
							if err != nil {
								t.Fatal(err)
							}
							rows, err := run(ctx, compiled)
							if err != nil {
								t.Fatal(err)
							}
							if st.Purpose == "totals" {
								got, err := obsquery.ParseTrafficTotals(rows)
								if err != nil {
									t.Fatal(err)
								}
								wantRequests := uint64(sc.End.Sub(sc.Start)/time.Hour) * 2
								if len(got) != 1 || got[0].Requests != wantRequests {
									t.Fatalf("rollup=%v totals=%+v, want %d requests", rollup, got, wantRequests)
								}
								if rollup && !reflect.DeepEqual(got, rawTotals) {
									t.Errorf("rollup totals=%+v, raw=%+v", got, rawTotals)
								}
								rawTotals = got
							} else {
								got, err := obsquery.ParseTrafficRows(rows)
								if err != nil {
									t.Fatal(err)
								}
								if len(got) == 0 || (rollup && !reflect.DeepEqual(got, rawRanks)) {
									t.Errorf("by=%q rollup=%v ranks=%+v, raw=%+v", by, rollup, got, rawRanks)
								}
								rawRanks = got
							}
						}
					}
				}
			})
		}
	}
}
