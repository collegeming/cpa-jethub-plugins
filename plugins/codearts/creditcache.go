package main

import (
	"strings"
	"sync"
	"time"

	"github.com/collegeming/cpa-jethub-plugins/internal/abiboot"
)

// creditCacheEntry remembers one account's balance + daily-activity pair.
// The two reads always come together on the status paths, so they share one
// slot and one timestamp; errors are cached with the pair — an upstream that
// is failing must not be hammered once per page load either.
type creditCacheEntry struct {
	Balance     *creditBalance
	BalanceErr  error
	Activity    *dailyActivity
	ActivityErr error
	Fetched     time.Time
}

var creditCache sync.Map // access_key_id -> creditCacheEntry

// cachedCreditSnapshot returns the account's balance and daily-login activity,
// remembered for the configured TTL (0 disables the cache entirely). The claim
// path bypasses this — a check-in must decide on live state — and invalidates
// the slot after a successful claim.
func cachedCreditSnapshot(h *abiboot.Host, credential *Credential) (*creditBalance, error, *dailyActivity, error) {
	ttl := time.Duration(settings().CreditCacheTTLMS) * time.Millisecond
	key := ""
	if credential != nil {
		key = strings.TrimSpace(credential.AccessKeyID)
	}
	if ttl <= 0 || key == "" {
		return creditSnapshot(h, credential)
	}
	if value, ok := creditCache.Load(key); ok {
		if entry, ok := value.(creditCacheEntry); ok && time.Since(entry.Fetched) < ttl {
			return entry.Balance, entry.BalanceErr, entry.Activity, entry.ActivityErr
		}
	}
	balance, balanceErr, activity, activityErr := creditSnapshot(h, credential)
	creditCache.Store(key, creditCacheEntry{
		Balance:     balance,
		BalanceErr:  balanceErr,
		Activity:    activity,
		ActivityErr: activityErr,
		Fetched:     time.Now(),
	})
	return balance, balanceErr, activity, activityErr
}

// creditSnapshot performs the two live reads in the order the status paths
// always used: the activity read is skipped when the balance read fails.
func creditSnapshot(h *abiboot.Host, credential *Credential) (*creditBalance, error, *dailyActivity, error) {
	balance, balanceErr := fetchCreditBalance(h, credential)
	if balanceErr != nil {
		return nil, balanceErr, nil, nil
	}
	activity, activityErr := fetchDailyActivity(h, credential)
	return balance, nil, activity, activityErr
}

// invalidateCredit drops one account's remembered figures so the read after a
// write (daily check-in claim) shows the fresh state.
func invalidateCredit(credential *Credential) {
	if credential == nil {
		return
	}
	if key := strings.TrimSpace(credential.AccessKeyID); key != "" {
		creditCache.Delete(key)
	}
}

// resetCreditCache clears every remembered account; tests use it so a stubbed
// host is not masked by an earlier test's reads.
func resetCreditCache() {
	creditCache.Range(func(key, _ any) bool {
		creditCache.Delete(key)
		return true
	})
}
