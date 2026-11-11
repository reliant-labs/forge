package obsbuiltin

import (
	"strings"
	"testing"
	"time"
)

// The engagement population: what the web runtime's page_engaged reports
// for a blog, checked through every panel that reads it. The range is the
// 30 days ending 2026-03-16 12:00 UTC, environment c1. Each visitor's rows:
//
//	hA  03-16 09:00 lands on /blog/launch from news.ycombinator.com, reads
//	    it 120 s to 95%, goes to /pricing (a route goal) at 09:02 and reads
//	    that 10 s; the backend's sign-up of uA at 09:04 carries hA.
//	hB  03-16 10:00 lands on /blog/launch from a newsletter (utm email),
//	    hides the tab after 30 s at 40%, comes back and leaves at 10:10: the
//	    resumed report says 90 s and 60% for the whole view. One view.
//	hC  03-16 11:00 lands on /blog/launch, leaves it after 5 s at 20% for
//	    /blog/other (60 s, 100%), and subscribes to the newsletter (an event
//	    goal) at 11:03.
//	hD  a bot reading /blog/launch for 999 s: never counted.
//	hE  03-14 10:00 lands on /blog/launch from t.co (200 s, 100%) and comes
//	    back an hour later, a new visit and a new view of it (10 s, 30%).
//	hF  03-15 09:00 lands on /blog, the index (15 s, 100%), which "/blog/*"
//	    does not match.
//
// And one page_engaged with no visitor hash (no edge): never counted.
const contentSeed = `
INSERT INTO forge_obsbuiltin_content.product_events
    (Timestamp, ServiceName, EnvId, UserId, EventName, Props, Client, DayVisitor, DeviceType) VALUES
    ('2026-03-16 09:00:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog/launch', 'entry': 'true', 'referrer_host': 'news.ycombinator.com'}, 'web', 'hA', 'desktop'),
    ('2026-03-16 09:02:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/pricing'}, 'web', 'hA', 'desktop'),
    ('2026-03-16 09:02:01', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '120000', 'scroll_pct': '95', 'entry': 'true'}, 'web', 'hA', 'desktop'),
    ('2026-03-16 09:04:00', 'api',  'c1', 'uA', 'auth_signed_up', {'method': 'password'}, 'backend', 'hA', ''),
    ('2026-03-16 09:06:00', 'blog', 'c1', '',   'page_engaged', {'route': '/pricing', 'engaged_ms': '10000', 'scroll_pct': '100'}, 'web', 'hA', 'desktop'),
    ('2026-03-16 10:00:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog/launch', 'entry': 'true', 'utm_source': 'newsletter', 'utm_medium': 'email'}, 'web', 'hB', 'mobile'),
    ('2026-03-16 10:01:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '30000', 'scroll_pct': '40', 'entry': 'true'}, 'web', 'hB', 'mobile'),
    ('2026-03-16 10:10:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '90000', 'scroll_pct': '60', 'entry': 'true', 'resumed': 'true'}, 'web', 'hB', 'mobile'),
    ('2026-03-16 11:00:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog/launch', 'entry': 'true'}, 'web', 'hC', 'desktop'),
    ('2026-03-16 11:01:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog/other'}, 'web', 'hC', 'desktop'),
    ('2026-03-16 11:01:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '5000', 'scroll_pct': '20', 'entry': 'true'}, 'web', 'hC', 'desktop'),
    ('2026-03-16 11:03:00', 'blog', 'c1', '',   'newsletter_signup', {}, 'web', 'hC', 'desktop'),
    ('2026-03-16 11:05:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/other', 'engaged_ms': '60000', 'scroll_pct': '100'}, 'web', 'hC', 'desktop'),
    ('2026-03-16 10:30:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog/launch', 'entry': 'true'}, 'web', 'hD', 'bot'),
    ('2026-03-16 10:31:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '999000', 'scroll_pct': '100', 'entry': 'true'}, 'web', 'hD', 'bot'),
    ('2026-03-14 10:00:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog/launch', 'entry': 'true', 'referrer_host': 't.co'}, 'web', 'hE', 'desktop'),
    ('2026-03-14 10:04:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '200000', 'scroll_pct': '100', 'entry': 'true'}, 'web', 'hE', 'desktop'),
    ('2026-03-14 11:00:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog/launch', 'entry': 'true'}, 'web', 'hE', 'desktop'),
    ('2026-03-14 11:01:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '10000', 'scroll_pct': '30', 'entry': 'true'}, 'web', 'hE', 'desktop'),
    ('2026-03-15 09:00:00', 'blog', 'c1', '',   'page_viewed',  {'route': '/blog', 'entry': 'true'}, 'web', 'hF', 'desktop'),
    ('2026-03-15 09:01:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog', 'engaged_ms': '15000', 'scroll_pct': '100', 'entry': 'true'}, 'web', 'hF', 'desktop'),
    ('2026-03-16 09:30:00', 'blog', 'c1', '',   'page_engaged', {'route': '/blog/launch', 'engaged_ms': '1000', 'scroll_pct': '100', 'entry': 'true'}, 'web', '', 'desktop');
`

// TestEngagementPanelsOnAPopulation checks every panel that reads
// page_engaged against answers worked by hand from the definitions.
func TestEngagementPanelsOnAPopulation(t *testing.T) {
	ch := newClickHouse(t, "forge_obsbuiltin_content", contentSeed)
	end := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)
	start := end.Add(-30 * 24 * time.Hour)
	blog := Input{Project: "blog", Web: true, Analytics: &Analytics{
		Goals:         []Goal{{ID: "newsletter", Event: "newsletter_signup"}, {ID: "pricing", Route: "/pricing"}},
		ContentRoutes: []string{"/blog/*"},
	}}
	defaults := Input{Project: "blog", Web: true}
	rows := func(r ...string) string { return strings.Join(r, "\n") }
	check := func(in Input, dash, id, want string) {
		t.Helper()
		d := Product(in)
		if dash == AcquisitionID {
			d = Acquisition(in)
		}
		p := panel(t, d, id)
		got, err := ch.try(p.SQL, start, end, "c1")
		if err != nil {
			t.Errorf("%s/%s: %v", dash, id, err)
			return
		}
		if got != want {
			t.Errorf("%s/%s =\n%s\nwant\n%s", dash, id, got, want)
		}
	}

	// Per view, /blog/launch's engaged seconds are hA 120, hB 90 (its
	// resumed report replaces the first), hC 5, hE 200 and hE 10 (two
	// views): sorted 5, 10, 90, 120, 200, so the median is 90 and the 75th
	// percentile (inclusive, at position 3 of 0..4) is 120. Most views
	// first, then by route. The bot and the unhashed report count nowhere.
	check(blog, ProductID, "engaged-time", rows(
		"/blog/launch\t90\t120", "/blog\t15\t15", "/blog/other\t60\t60", "/pricing\t10\t10"))

	// Content, "/blog/*" only. /blog/launch: 5 views by 4 visitor-days;
	// depth 95, 60, 20, 100, 30, so 3 of 5 read half and 2 of 5 read 90%;
	// all 5 visits that saw it entered on it, and hB's and both of hE's
	// ended on it; hA went on to /pricing (a goal) and signed up, hC
	// subscribed: 2 of 5 visits converted onward. /blog/other: hC's view,
	// the visit's exit, followed by the newsletter goal.
	check(blog, ProductID, "content", rows(
		"/blog/launch\t5\t4\t90\t60\t40\t5\t3\t2\t40",
		"/blog/other\t1\t1\t60\t100\t100\t0\t1\t1\t100"))
	// With nothing declared every route is content, and only a sign-up
	// converts: hA's (after both /blog/launch and /pricing).
	check(defaults, ProductID, "content", rows(
		"/blog/launch\t5\t4\t90\t60\t40\t5\t3\t1\t20",
		"/blog\t1\t1\t15\t100\t100\t1\t1\t0\t0",
		"/blog/other\t1\t1\t60\t100\t100\t0\t1\t0\t0",
		"/pricing\t1\t1\t10\t100\t100\t0\t1\t1\t100"))

	// Landing pages: hA (news.ycombinator.com) and hE (t.co) are Organic
	// social, three visits between them and hA's sign-up; their landing
	// views held them 120 s and 200 s (hE's first view of the day), a
	// median of 160. hB is Email (90 s), hC and hF Direct.
	check(blog, AcquisitionID, "landing-pages", rows(
		"/blog/launch\tOrganic social\t3\t1\t160",
		"/blog\tDirect\t1\t0\t15",
		"/blog/launch\tDirect\t1\t0\t5",
		"/blog/launch\tEmail\t1\t0\t90"))

	// A page_engaged is not interaction: it starts no visit and keeps none
	// from being a bounce. hB's visit is one page view and two reports: a
	// bounce, with hE's two visits and hF's; hA's and hC's are not. 4 of 6.
	check(blog, ProductID, "bounce", "0.6667")
}
