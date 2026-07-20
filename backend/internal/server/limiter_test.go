package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

func TestWindowLimiterFixedWindowAndCleanup(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newWindowLimiter()
	limiter.now = func() time.Time { return now }
	if allowed, _ := limiter.Allow("a", 1, time.Minute); !allowed {
		t.Fatalf("expected first request allowed")
	}
	if allowed, retry := limiter.Allow("a", 1, time.Minute); allowed || retry != time.Minute {
		t.Fatalf("expected second request limited, allowed=%v retry=%v", allowed, retry)
	}
	for i := 0; i < 100; i++ {
		limiter.Allow(string(rune('b'+i)), 1, time.Minute)
	}
	now = now.Add(2 * time.Minute)
	if allowed, _ := limiter.Allow("fresh", 1, time.Minute); !allowed {
		t.Fatalf("expected fresh request allowed")
	}
	if limiter.size() != 1 {
		t.Fatalf("expected expired map entries cleaned, size=%d", limiter.size())
	}
}

func TestWindowLimiterZeroDisablesLimit(t *testing.T) {
	limiter := newWindowLimiter()
	for i := 0; i < 100; i++ {
		if allowed, _ := limiter.Allow("same", 0, time.Minute); !allowed {
			t.Fatalf("zero limit must disable limiter")
		}
	}
}

func TestWindowLimiterSessionLimitDoesNotConsumeGlobalAndHasNoSideEffects(t *testing.T) {
	limiter := newWindowLimiter()
	specs := []limitSpec{{Key: "token-global", Limit: 10, Window: time.Minute}, {Key: "token-session:one", Limit: 1, Window: time.Minute}}
	if allowed, _ := limiter.AllowMany(specs); !allowed {
		t.Fatalf("expected first multi-bucket request allowed")
	}
	if allowed, _ := limiter.AllowMany(specs); allowed {
		t.Fatalf("expected session bucket to reject second request")
	}
	if limiter.count("token-global") != 1 || limiter.count("token-session:one") != 1 {
		t.Fatalf("rejected multi-bucket request must not increment any bucket: global=%d session=%d", limiter.count("token-global"), limiter.count("token-session:one"))
	}
	if allowed, _ := limiter.AllowMany([]limitSpec{{Key: "duplicate", Limit: 2, Window: time.Minute}, {Key: "duplicate", Limit: 2, Window: time.Minute}}); !allowed || limiter.count("duplicate") != 1 {
		t.Fatalf("duplicate key must be incremented once")
	}
}

func TestWindowLimiterPublicIPRejectionDoesNotCreateOtherBuckets(t *testing.T) {
	limiter := newWindowLimiter()
	first := []limitSpec{{Key: "lease-global", Limit: 0, Window: time.Minute}, {Key: "owner:a", Limit: 10, Window: time.Minute}, {Key: "ip:shared", Limit: 1, Window: time.Minute}}
	if allowed, _ := limiter.AllowMany(first); !allowed {
		t.Fatalf("expected first public lease admission")
	}
	second := []limitSpec{{Key: "lease-global", Limit: 0, Window: time.Minute}, {Key: "owner:b", Limit: 10, Window: time.Minute}, {Key: "ip:shared", Limit: 1, Window: time.Minute}}
	if allowed, _ := limiter.AllowMany(second); allowed {
		t.Fatalf("expected shared public IP limit")
	}
	if limiter.count("owner:b") != 0 || limiter.count("lease-global") != 0 || limiter.size() != 2 {
		t.Fatalf("IP rejection created side effects: owner=%d global=%d size=%d", limiter.count("owner:b"), limiter.count("lease-global"), limiter.size())
	}
}

func TestWindowLimiterUsesPerEntryExpiryAndDisableClearsState(t *testing.T) {
	now := time.Unix(100, 0)
	limiter := newWindowLimiter()
	limiter.now = func() time.Time { return now }
	limiter.Allow("long", 2, 10*time.Minute)
	limiter.Allow("short", 2, time.Minute)
	now = now.Add(2 * time.Minute)
	limiter.Allow("new-short", 2, time.Minute)
	if limiter.count("long") != 1 || limiter.count("short") != 0 {
		t.Fatalf("different windows cleaned incorrectly: long=%d short=%d", limiter.count("long"), limiter.count("short"))
	}
	if allowed, _ := limiter.Allow("toggle", 1, time.Minute); !allowed {
		t.Fatalf("expected enabled limit allowed")
	}
	if allowed, _ := limiter.Allow("toggle", 0, time.Minute); !allowed || limiter.count("toggle") != 0 {
		t.Fatalf("disabling must clear old state")
	}
	if allowed, _ := limiter.Allow("toggle", 1, time.Minute); !allowed {
		t.Fatalf("re-enabled limit must not revive old state")
	}
}

func TestWindowLimiterHasHardEntryBound(t *testing.T) {
	limiter := newWindowLimiterWithMaxEntries(2)
	if allowed, _ := limiter.Allow("one", 10, time.Minute); !allowed {
		t.Fatalf("first key rejected")
	}
	if allowed, _ := limiter.Allow("two", 10, time.Minute); !allowed {
		t.Fatalf("second key rejected")
	}
	if allowed, retry := limiter.Allow("three", 10, time.Minute); allowed || retry <= 0 {
		t.Fatalf("entry bound did not reject new key: allowed=%v retry=%v", allowed, retry)
	}
	if limiter.size() != 2 {
		t.Fatalf("entry bound exceeded: %d", limiter.size())
	}
	if allowed, _ := limiter.Allow("one", 10, time.Minute); !allowed {
		t.Fatalf("existing key should remain usable at capacity")
	}
}

func TestBlockedLoginIPDoesNotConsumeGlobalBucket(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.Abuse.Login.GlobalPerMinute = 1
	cfg.Abuse.Login.IPMaxFailures = 1
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.DB.Close()
	s := &Server{config: cfg, store: st, loginLimiter: newLoginLimiter(), rateLimiter: newWindowLimiter()}
	s.recordLoginFailure("user", "198.51.100.1")
	app := fiber.New(fiber.Config{ErrorHandler: jsonErrorHandler})
	app.Get("/", func(c *fiber.Ctx) error { return s.checkLoginAdmission(c, "user", "198.51.100.1") })
	for i := 0; i < 3; i++ {
		resp, requestErr := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
		if requestErr != nil {
			t.Fatalf("blocked request: %v", requestErr)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("expected blocked response, got %d", resp.StatusCode)
		}
	}
	if s.rateLimiter.count("login-global:user") != 0 {
		t.Fatalf("blocked IP requests must not consume global bucket")
	}
}
