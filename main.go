package main

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	geth "github.com/ethereum/go-ethereum/crypto"
	"github.com/joho/godotenv"
)

type Config struct {
	Address        string // EOA / V1 trader address
	ProxyAddress   string // V2 Polymarket proxy (holds pUSD, owner of V2 trades)
	APIKey         string
	Secret         string
	Passphrase     string
	PrivateKey     *ecdsa.PrivateKey
	RelayerAPIKey  string
	RelayerKeyAddr string
}

func main() {
	_ = godotenv.Load()

	var privKey *ecdsa.PrivateKey
	if pk := os.Getenv("POLYMARKET_PRIVATE_KEY"); pk != "" {
		var err error
		privKey, err = geth.HexToECDSA(strings.TrimPrefix(pk, "0x"))
		if err != nil {
			log.Printf("Warning: invalid POLYMARKET_PRIVATE_KEY: %v", err)
		}
	}

	addr := os.Getenv("POLYMARKET_ADDRESS")
	if addr == "" {
		log.Fatal("POLYMARKET_ADDRESS is required (see .env.example)")
	}
	proxyAddr := os.Getenv("POLYMARKET_PROXY_ADDRESS")
	if proxyAddr == "" {
		log.Fatal("POLYMARKET_PROXY_ADDRESS is required (see .env.example)")
	}
	cfg := Config{
		Address:        addr,
		ProxyAddress:   proxyAddr,
		APIKey:         os.Getenv("CLOB_API_KEY"),
		Secret:         os.Getenv("CLOB_SECRET"),
		Passphrase:     os.Getenv("CLOB_PASSPHRASE"),
		PrivateKey:     privKey,
		RelayerAPIKey:  os.Getenv("RELAYER_API_KEY"),
		RelayerKeyAddr: os.Getenv("RELAYER_API_KEY_ADDRESS"),
	}

	// API endpoints for frontend.
	http.HandleFunc("/api/positions", func(w http.ResponseWriter, r *http.Request) {
		data := fetchPositionsAll(cfg)
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	http.HandleFunc("/api/trades", func(w http.ResponseWriter, r *http.Request) {
		data := fetchTrades(cfg)
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})

	http.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		summary := buildSummary(cfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(summary)
	})

	http.HandleFunc("/api/all-time-activity", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(buildAllTimeActivity(cfg))
	})

	http.HandleFunc("/api/balance", func(w http.ResponseWriter, r *http.Request) {
		bal := fetchBalanceCombined(cfg)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bal)
	})

	http.HandleFunc("/api/real-profit", func(w http.ResponseWriter, r *http.Request) {
		var positions []Position
		json.Unmarshal(fetchPositionsAll(cfg), &positions)

		// Polymarket markets are denominated in ET, and every other handler
		// (today-positions, all-time-activity) buckets days in America/New_York.
		// Use ET here too so "today" agrees across the dashboard rather than
		// following the server's local timezone.
		etLoc, err := time.LoadLocation("America/New_York")
		if err != nil {
			etLoc = time.UTC
		}
		today := time.Now().In(etLoc).Format("2006-01-02")

		var rp RealProfit
		rp.AllTimePositions = len(positions)
		for _, p := range positions {
			rp.AllTimeInvested += p.InitialVal
			rp.AllTimeCurrentValue += p.CurrentVal
			rp.AllTimeUnrealizedPnl += p.CashPnl
			rp.AllTimeRealizedPnl += p.RealizedPnl

			// Unredeemed winning positions (curPrice=1 = resolved winner).
			if p.CurPrice >= 0.99 {
				rp.RedeemableValue += p.CurrentVal
				rp.RedeemableCount++
			}

			if strings.HasPrefix(p.EndDate, today) {
				rp.TodayPositions++
				rp.TodayInvested += p.InitialVal
				rp.TodayCurrentValue += p.CurrentVal
				rp.TodayPnl += p.CashPnl
				if p.CurPrice >= 0.99 {
					rp.TodayResolved++
					rp.TodayWins++
				} else if p.CurPrice <= 0.01 {
					rp.TodayResolved++
					rp.TodayLosses++
					rp.TodayLostAmount += p.InitialVal
				} else {
					rp.TodayOpen++
				}
			}
		}
		rp.AllTimeTotalPnl = rp.AllTimeUnrealizedPnl + rp.AllTimeRealizedPnl

		// Wallet balance for "Total Assets" card.
		bal := fetchBalanceCombined(cfg)
		rp.WalletUSDC = bal.USDC + bal.USDCe + bal.PUSD

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rp)
	})

	http.HandleFunc("/api/today-positions", func(w http.ResponseWriter, r *http.Request) {
		// Use ET (Eastern Time) since Polymarket markets use ET time slots.
		etLoc, err := time.LoadLocation("America/New_York")
		if err != nil {
			etLoc = time.UTC
		}
		nowET := time.Now().In(etLoc)
		startOfDay := time.Date(nowET.Year(), nowET.Month(), nowET.Day(), 0, 0, 0, 0, etLoc).Unix()

		// 1. Current positions with today's endDate.
		var positions []Position
		json.Unmarshal(fetchPositionsAll(cfg), &positions)

		// 2. Today's BUY events (for timestamps and invested amounts).
		type buyInfo struct {
			Timestamp int64
			Invested  float64
		}
		buys := map[string]buyInfo{} // title → info
		for offset := 0; ; offset += 500 {
			events, _ := fetchActivityPageAll(cfg, "TRADE", 500, offset)
			if len(events) == 0 {
				break
			}
			for _, e := range events {
				if e.Timestamp >= startOfDay && e.Side == "BUY" {
					bi := buys[e.Title]
					bi.Invested += e.UsdcSize
					if e.Timestamp > bi.Timestamp {
						bi.Timestamp = e.Timestamp
					}
					buys[e.Title] = bi
				}
			}
			if len(events) < 500 {
				break
			}
		}

		// 3. Today's REDEEM events (for redeemed wins).
		type redeemInfo struct {
			UsdcSize  float64
			Timestamp int64
		}
		redeemed := map[string]redeemInfo{} // title → info
		for offset := 0; ; offset += 500 {
			events, _ := fetchActivityPageAll(cfg, "REDEEM", 500, offset)
			if len(events) == 0 {
				break
			}
			for _, e := range events {
				if e.Timestamp >= startOfDay {
					redeemed[e.Title] = redeemInfo{e.UsdcSize, e.Timestamp}
				}
			}
			if len(events) < 500 {
				break
			}
		}

		// 4. Build result from positions with today's endDate.
		var result []TodayPosition
		seen := map[string]bool{}

		for _, p := range positions {
			if _, boughtToday := buys[p.Title]; !boughtToday {
				continue
			}
			status := "PENDING"
			if p.CurPrice >= 0.99 {
				status = "WON"
			} else if p.CurPrice <= 0.01 {
				status = "LOST"
			}
			// Use BUY timestamp for ordering (newest purchase first).
			ts := buys[p.Title].Timestamp
			result = append(result, TodayPosition{
				Title:     p.Title,
				Outcome:   p.Outcome,
				Size:      p.Size,
				Invested:  p.InitialVal,
				Value:     p.CurrentVal,
				Pnl:       p.CashPnl,
				Status:    status,
				Timestamp: ts,
			})
			seen[p.Title] = true
		}

		// 5. Add redeemed positions that disappeared from the positions API.
		for title, ri := range redeemed {
			if seen[title] {
				continue
			}
			invested := float64(0)
			ts := ri.Timestamp
			if bi, ok := buys[title]; ok {
				invested = bi.Invested
				if bi.Timestamp > ts {
					ts = bi.Timestamp
				}
			}
			pnl := ri.UsdcSize - invested
			result = append(result, TodayPosition{
				Title:     title,
				Size:      ri.UsdcSize,
				Value:     ri.UsdcSize,
				Invested:  invested,
				Pnl:       pnl,
				Status:    "WON",
				Timestamp: ts,
			})
		}

		// 6. Sort by timestamp desc (newest first).
		sort.Slice(result, func(i, j int) bool {
			return result[i].Timestamp > result[j].Timestamp
		})

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	// Manual redeem endpoints. Polymarket V2 auto-redeems winners on the proxy,
	// but with delay (sometimes hours). These endpoints give users a button to
	// claim instantly without waiting.
	http.HandleFunc("/api/redeemable", func(w http.ResponseWriter, r *http.Request) {
		positions, err := fetchRedeemablePositions(cfg.Address)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		var wins []RedeemablePosition
		for _, pos := range positions {
			winnerIdx, resolved := checkResolution(pos.ConditionID)
			if resolved && winnerIdx == pos.OutcomeIndex {
				wins = append(wins, pos)
			}
		}
		json.NewEncoder(w).Encode(wins)
	})

	http.HandleFunc("/api/redeem", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if cfg.PrivateKey == nil {
			json.NewEncoder(w).Encode(map[string]string{"error": "No POLYMARKET_PRIVATE_KEY configured"})
			return
		}
		rcfg := RedeemConfig{
			PrivateKey: cfg.PrivateKey,
			SignerAddr: cfg.Address,
		}
		redeemMu.Lock()
		results, err := redeemAll(rcfg)
		redeemMu.Unlock()
		if err != nil {
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		if results == nil {
			json.NewEncoder(w).Encode(map[string]string{"message": "No positions to redeem"})
			return
		}
		json.NewEncoder(w).Encode(results)
	})

	// Serve the dashboard HTML.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	// Auto-redeem every 25 minutes.
	// if cfg.PrivateKey != nil {
	// 	go func() {
	// 		rcfg := RedeemConfig{
	// 			PrivateKey: cfg.PrivateKey,
	// 			SignerAddr: cfg.Address,
	// 		}
	// 		for {
	// 			func() {
	// 				defer func() {
	// 					if rec := recover(); rec != nil {
	// 						log.Printf("[auto-redeem] Panic recovered: %v", rec)
	// 					}
	// 				}()
	// 				log.Println("[auto-redeem] Checking for redeemable positions...")
	// 				redeemMu.Lock()
	// 				results, err := redeemAll(rcfg)
	// 				redeemMu.Unlock()
	// 				if err != nil {
	// 					log.Printf("[auto-redeem] Error: %v", err)
	// 				} else if results == nil {
	// 					log.Println("[auto-redeem] No positions to redeem")
	// 				} else {
	// 					for _, r := range results {
	// 						if r.Success {
	// 							log.Printf("[auto-redeem] Redeemed %s (%.2f shares) tx=%s", r.Title, r.Shares, r.TxHash)
	// 						} else {
	// 							log.Printf("[auto-redeem] Failed %s: %s", r.Title, r.Error)
	// 						}
	// 					}
	// 				}
	// 			}()
	// 			time.Sleep(25 * time.Minute)
	// 		}
	// 	}()
	// 	log.Println("Auto-redeem enabled (every 25 minutes)")
	// } else {
	// 	log.Println("Auto-redeem disabled (no POLYMARKET_PRIVATE_KEY)")
	// }

	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	log.Printf("Dashboard running at http://localhost:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Position from the data API.
type Position struct {
	Asset       string  `json:"asset"`
	ConditionID string  `json:"conditionId"`
	Title       string  `json:"title"`
	Size        float64 `json:"size"`
	AvgPrice    float64 `json:"avgPrice"`
	CurPrice    float64 `json:"curPrice"`
	InitialVal  float64 `json:"initialValue"`
	CurrentVal  float64 `json:"currentValue"`
	CashPnl     float64 `json:"cashPnl"`
	PercentPnl  float64 `json:"percentPnl"`
	RealizedPnl float64 `json:"realizedPnl"`
	Redeemable  bool    `json:"redeemable"`
	Outcome     string  `json:"outcome"`
	EndDate     string  `json:"endDate"`
}

type Trade struct {
	ID         string `json:"id"`
	Market     string `json:"market"`
	Side       string `json:"side"`
	Price      string `json:"price"`
	Size       string `json:"size"`
	Outcome    string `json:"outcome"`
	Status     string `json:"status"`
	MatchTime  string `json:"match_time"`
	FeeRateBps string `json:"fee_rate_bps"`
	TxHash     string `json:"transaction_hash"`
	Title      string `json:"title"`
}

type Summary struct {
	TotalPositions int     `json:"totalPositions"`
	TotalInvested  float64 `json:"totalInvested"`
	CurrentValue   float64 `json:"currentValue"`
	UnrealizedPnl  float64 `json:"unrealizedPnl"`
	RealizedPnl    float64 `json:"realizedPnl"`
	TotalPnl       float64 `json:"totalPnl"`
	TotalTrades    int     `json:"totalTrades"`
	WinCount       int     `json:"winCount"`
	LossCount      int     `json:"lossCount"`
	PendingCount   int     `json:"pendingCount"`
	WinRate        float64 `json:"winRate"`
	AvgEntry       float64 `json:"avgEntry"`
	UsdcBalance    float64 `json:"usdcBalance"`
}

type Balance struct {
	USDC  float64 `json:"usdc"`
	USDCe float64 `json:"usdce"`
	PUSD  float64 `json:"pusd"`
	POL   float64 `json:"pol"`
}

type RealProfit struct {
	TodayInvested        float64 `json:"todayInvested"`
	TodayCurrentValue    float64 `json:"todayCurrentValue"`
	TodayPnl             float64 `json:"todayPnl"`
	TodayPositions       int     `json:"todayPositions"`
	TodayResolved        int     `json:"todayResolved"`
	TodayWins            int     `json:"todayWins"`
	TodayLosses          int     `json:"todayLosses"`
	TodayLostAmount      float64 `json:"todayLostAmount"`
	TodayOpen            int     `json:"todayOpen"`
	AllTimeInvested      float64 `json:"allTimeInvested"`
	AllTimeCurrentValue  float64 `json:"allTimeCurrentValue"`
	AllTimeUnrealizedPnl float64 `json:"allTimeUnrealizedPnl"`
	AllTimeRealizedPnl   float64 `json:"allTimeRealizedPnl"`
	AllTimeTotalPnl      float64 `json:"allTimeTotalPnl"`
	AllTimePositions     int     `json:"allTimePositions"`
	RedeemableValue      float64 `json:"redeemableValue"`
	RedeemableCount      int     `json:"redeemableCount"`
	WalletUSDC           float64 `json:"walletUSDC"` // liquid USDC in wallet right now
}

type TodayPosition struct {
	Title     string  `json:"title"`
	Outcome   string  `json:"outcome"`
	Size      float64 `json:"size"`
	Invested  float64 `json:"invested"`
	Value     float64 `json:"value"`
	Pnl       float64 `json:"pnl"`
	Status    string  `json:"status"`
	Timestamp int64   `json:"timestamp"`
}

// --- All-time activity types ---

type DailyActivity struct {
	Date     string  `json:"date"`
	Bets     int     `json:"bets"`
	Wins     int     `json:"wins"`
	Invested float64 `json:"invested"`
	Redeemed float64 `json:"redeemed"`
}

type AssetActivity struct {
	Asset    string  `json:"asset"`
	Bets     int     `json:"bets"`
	Wins     int     `json:"wins"`
	Invested float64 `json:"invested"`
	Redeemed float64 `json:"redeemed"`
}

type AllTimeActivity struct {
	TotalBets     int             `json:"totalBets"`
	TotalWins     int             `json:"totalWins"`
	TotalLosses   int             `json:"totalLosses"`
	TotalPending  int             `json:"totalPending"`
	TotalInvested float64         `json:"totalInvested"`
	TotalRedeemed float64         `json:"totalRedeemed"`
	NetPnl        float64         `json:"netPnl"`
	WinRate       float64         `json:"winRate"`
	ByDate        []DailyActivity `json:"byDate"`
	ByAsset       []AssetActivity `json:"byAsset"`
}

// activityEvent is a single entry from the data-api activity endpoint.
type activityEvent struct {
	Timestamp int64   `json:"timestamp"`
	Type      string  `json:"type"`
	Side      string  `json:"side"`
	UsdcSize  float64 `json:"usdcSize"`
	Title     string  `json:"title"`
}

// fetchActivityPage returns one page of activity events.
func fetchActivityPage(address, actType string, limit, offset int) ([]activityEvent, error) {
	u := fmt.Sprintf("https://data-api.polymarket.com/activity?user=%s&type=%s&limit=%d&offset=%d",
		address, actType, limit, offset)
	resp, err := http.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var events []activityEvent
	json.Unmarshal(body, &events)
	return events, nil
}

// titleTimeRe matches time ranges like "5:15PM-5:20PM" in market titles.
var titleTimeRe = regexp.MustCompile(`(\d{1,2}:\d{2}(?:AM|PM))\s*-\s*(\d{1,2}:\d{2}(?:AM|PM))`)

// parseSlotTime extracts the end time from a title like "BTC Up or Down - April 17, 5:15PM-5:20PM ET"
// and returns it as a Unix timestamp for today. Returns 0 if no match.
func parseSlotTime(title string, today time.Time) int64 {
	m := titleTimeRe.FindStringSubmatch(title)
	if m == nil {
		return 0
	}
	// Use the end time (m[2]) for ordering — that's when the bet resolves.
	endTimeStr := m[2]
	t, err := time.Parse("3:04PM", endTimeStr)
	if err != nil {
		return 0
	}
	// Combine with today's date. Times are ET (America/New_York).
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	full := time.Date(today.Year(), today.Month(), today.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	return full.Unix()
}

// assetFromTitle extracts a short ticker from a Polymarket market title.
// Crypto up/down markets are titled by ticker ("BTC Up or Down - ...") rather
// than full name, so match both the ticker prefix and the spelled-out name to
// avoid bucketing everything into "Other".
func assetFromTitle(title string) string {
	t := strings.ToUpper(title)
	switch {
	case strings.HasPrefix(t, "BTC") || strings.Contains(t, "BITCOIN"):
		return "BTC"
	case strings.HasPrefix(t, "ETH") || strings.Contains(t, "ETHEREUM"):
		return "ETH"
	case strings.HasPrefix(t, "SOL") || strings.Contains(t, "SOLANA"):
		return "SOL"
	case strings.HasPrefix(t, "BNB") || strings.Contains(t, "BINANCE COIN"):
		return "BNB"
	case strings.HasPrefix(t, "XRP") || strings.Contains(t, "RIPPLE"):
		return "XRP"
	default:
		return "Other"
	}
}

// tsToDate converts a Unix second timestamp to "YYYY-MM-DD" in America/New_York.
// Polymarket markets are titled in ET, so day buckets must align with ET to
// match what /api/today-positions reports.
func tsToDate(ts int64) string {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		loc = time.UTC
	}
	return time.Unix(ts, 0).In(loc).Format("2006-01-02")
}

// fetchAllActivityFor pages through every event of a given type for one address.
func fetchAllActivityFor(address, actType string) []activityEvent {
	const pageSize = 500
	var all []activityEvent
	for offset := 0; ; offset += pageSize {
		ev, _ := fetchActivityPage(address, actType, pageSize, offset)
		all = append(all, ev...)
		if len(ev) < pageSize {
			break
		}
	}
	return all
}

// Cache for buildAllTimeActivity — this endpoint touches the network many times,
// so even a 30s TTL turns the typical "click refresh repeatedly" pattern into one
// expensive call followed by instant hits.
var (
	allTimeMu    sync.Mutex
	allTimeVal   *AllTimeActivity
	allTimeT     time.Time
	allTimeTTL   = 30 * time.Second
)

// buildAllTimeActivity computes all-time win/loss stats from the activity log.
// This is the source of truth: positions disappear after redemption, but activity
// events are permanent. Pages activity from both EOA and V2 proxy in parallel.
func buildAllTimeActivity(cfg Config) AllTimeActivity {
	allTimeMu.Lock()
	if allTimeVal != nil && time.Since(allTimeT) < allTimeTTL {
		v := *allTimeVal
		allTimeMu.Unlock()
		return v
	}
	allTimeMu.Unlock()

	// Map date → stats, asset → stats.
	dateMap := map[string]*DailyActivity{}
	assetMap := map[string]*AssetActivity{}

	ensure := func(date, asset string) {
		if _, ok := dateMap[date]; !ok {
			dateMap[date] = &DailyActivity{Date: date}
		}
		if _, ok := assetMap[asset]; !ok {
			assetMap[asset] = &AssetActivity{Asset: asset}
		}
	}

	var totalBets, totalWins int
	var totalInvested, totalRedeemed float64

	// Fetch TRADE + REDEEM for EOA and proxy concurrently (4 streams + positions).
	addrs := []string{cfg.Address, cfg.ProxyAddress}
	trades := make([][]activityEvent, len(addrs))
	redeems := make([][]activityEvent, len(addrs))
	var positions []Position
	var wg sync.WaitGroup
	for i, addr := range addrs {
		if addr == "" {
			continue
		}
		wg.Add(2)
		go func(i int, addr string) {
			defer wg.Done()
			trades[i] = fetchAllActivityFor(addr, "TRADE")
		}(i, addr)
		go func(i int, addr string) {
			defer wg.Done()
			redeems[i] = fetchAllActivityFor(addr, "REDEEM")
		}(i, addr)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		json.Unmarshal(fetchPositionsAll(cfg), &positions)
	}()
	wg.Wait()

	for _, page := range trades {
		for _, e := range page {
			if e.Side != "BUY" {
				continue
			}
			date := tsToDate(e.Timestamp)
			asset := assetFromTitle(e.Title)
			ensure(date, asset)
			dateMap[date].Bets++
			dateMap[date].Invested += e.UsdcSize
			assetMap[asset].Bets++
			assetMap[asset].Invested += e.UsdcSize
			totalBets++
			totalInvested += e.UsdcSize
		}
	}

	for _, page := range redeems {
		for _, e := range page {
			date := tsToDate(e.Timestamp)
			asset := assetFromTitle(e.Title)
			ensure(date, asset)
			dateMap[date].Wins++
			dateMap[date].Redeemed += e.UsdcSize
			assetMap[asset].Wins++
			assetMap[asset].Redeemed += e.UsdcSize
			totalWins++
			totalRedeemed += e.UsdcSize
		}
	}

	// 3. Pending = current open positions (curPrice between 0.01 and 0.99).
	totalPending := 0
	for _, p := range positions {
		if p.CurPrice > 0.01 && p.CurPrice < 0.99 {
			totalPending++
		}
	}

	totalLosses := totalBets - totalWins - totalPending
	if totalLosses < 0 {
		totalLosses = 0
	}

	settled := totalWins + totalLosses
	winRate := 0.0
	if settled > 0 {
		winRate = float64(totalWins) / float64(settled) * 100
	}

	// Sort dates.
	dates := make([]string, 0, len(dateMap))
	for d := range dateMap {
		dates = append(dates, d)
	}
	sortStrings(dates)

	byDate := make([]DailyActivity, len(dates))
	for i, d := range dates {
		byDate[i] = *dateMap[d]
	}

	assets := make([]string, 0, len(assetMap))
	for a := range assetMap {
		assets = append(assets, a)
	}
	sortStrings(assets)

	byAsset := make([]AssetActivity, len(assets))
	for i, a := range assets {
		byAsset[i] = *assetMap[a]
	}

	result := AllTimeActivity{
		TotalBets:     totalBets,
		TotalWins:     totalWins,
		TotalLosses:   totalLosses,
		TotalPending:  totalPending,
		TotalInvested: totalInvested,
		TotalRedeemed: totalRedeemed,
		NetPnl:        totalRedeemed - totalInvested,
		WinRate:       winRate,
		ByDate:        byDate,
		ByAsset:       byAsset,
	}
	allTimeMu.Lock()
	allTimeVal = &result
	allTimeT = time.Now()
	allTimeMu.Unlock()
	return result
}

// sortStrings sorts a string slice in place.
func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}

// fetchPositions pages through ALL positions for an address (100 per page).
// Works correctly even with 10 000+ bets — no hardcoded limit to update.
func fetchPositions(address string) []byte {
	const pageSize = 100
	var all []json.RawMessage
	for offset := 0; ; offset += pageSize {
		u := fmt.Sprintf("https://data-api.polymarket.com/positions?user=%s&limit=%d&offset=%d", address, pageSize, offset)
		resp, err := http.Get(u)
		if err != nil {
			break
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var page []json.RawMessage
		if json.Unmarshal(body, &page) != nil || len(page) == 0 {
			break
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break // last page
		}
	}
	if len(all) == 0 {
		return []byte("[]")
	}
	result, _ := json.Marshal(all)
	return result
}

// Short-lived response cache. A single dashboard refresh fires 7 endpoints in
// parallel and several of them re-page the same data. Caching for a few seconds
// collapses that to one fetch per data source.
var (
	posCacheMu   sync.Mutex
	posCache     []byte
	posCacheT    time.Time
	actCacheMu   sync.Mutex
	actCache     = map[string][]activityEvent{}
	actCacheT    = map[string]time.Time{}
	cacheTTL     = 5 * time.Second
)

// fetchPositionsAll merges positions from V1 EOA and V2 proxy in parallel.
// After the April 28 2026 V2 migration, new trades are recorded under the proxy.
func fetchPositionsAll(cfg Config) []byte {
	posCacheMu.Lock()
	if posCache != nil && time.Since(posCacheT) < cacheTTL {
		defer posCacheMu.Unlock()
		return posCache
	}
	posCacheMu.Unlock()
	addrs := []string{cfg.Address, cfg.ProxyAddress}
	results := make([][]json.RawMessage, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		if addr == "" {
			continue
		}
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			var page []json.RawMessage
			json.Unmarshal(fetchPositions(addr), &page)
			results[i] = page
		}(i, addr)
	}
	wg.Wait()
	var merged []json.RawMessage
	for _, r := range results {
		merged = append(merged, r...)
	}
	var result []byte
	if len(merged) == 0 {
		result = []byte("[]")
	} else {
		result, _ = json.Marshal(merged)
	}
	posCacheMu.Lock()
	posCache = result
	posCacheT = time.Now()
	posCacheMu.Unlock()
	return result
}

// fetchActivityPageAll fetches one page of activity from EOA and proxy in parallel.
func fetchActivityPageAll(cfg Config, actType string, limit, offset int) ([]activityEvent, error) {
	key := fmt.Sprintf("%s|%d|%d", actType, limit, offset)
	actCacheMu.Lock()
	if ev, ok := actCache[key]; ok && time.Since(actCacheT[key]) < cacheTTL {
		actCacheMu.Unlock()
		return ev, nil
	}
	actCacheMu.Unlock()

	addrs := []string{cfg.Address, cfg.ProxyAddress}
	results := make([][]activityEvent, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		if addr == "" {
			continue
		}
		wg.Add(1)
		go func(i int, addr string) {
			defer wg.Done()
			ev, _ := fetchActivityPage(addr, actType, limit, offset)
			results[i] = ev
		}(i, addr)
	}
	wg.Wait()
	var merged []activityEvent
	for _, r := range results {
		merged = append(merged, r...)
	}
	actCacheMu.Lock()
	actCache[key] = merged
	actCacheT[key] = time.Now()
	actCacheMu.Unlock()
	return merged, nil
}

// clobGet performs one authenticated GET to the CLOB API and returns the body.
func clobGet(cfg Config, path string) ([]byte, error) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signHMAC(cfg.Secret, ts, "GET", path)
	req, _ := http.NewRequest("GET", "https://clob.polymarket.com"+path, nil)
	req.Header.Set("POLY_ADDRESS", cfg.Address)
	req.Header.Set("POLY_API_KEY", cfg.APIKey)
	req.Header.Set("POLY_SIGNATURE", sig)
	req.Header.Set("POLY_TIMESTAMP", ts)
	req.Header.Set("POLY_PASSPHRASE", cfg.Passphrase)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// fetchTrades pages through ALL trades using the CLOB cursor pagination.
// "LTE=" is the Polymarket sentinel meaning "no more pages".
func fetchTrades(cfg Config) []byte {
	type tradePage struct {
		Data       []Trade `json:"data"`
		NextCursor string  `json:"next_cursor"`
	}
	var all []Trade
	cursor := ""
	for {
		path := "/trades"
		if cursor != "" {
			path += "?next_cursor=" + cursor
		}
		body, err := clobGet(cfg, path)
		if err != nil {
			break
		}
		var page tradePage
		if json.Unmarshal(body, &page) != nil {
			break
		}
		all = append(all, page.Data...)
		if page.NextCursor == "" || page.NextCursor == "LTE=" {
			break
		}
		cursor = page.NextCursor
	}

	// Enrich trades with market titles from positions data.
	var positions []Position
	json.Unmarshal(fetchPositions(cfg.Address), &positions)
	titleMap := make(map[string]string)
	for _, p := range positions {
		titleMap[p.ConditionID] = p.Title
	}
	for i := range all {
		if t, ok := titleMap[all[i].Market]; ok {
			all[i].Title = t
		}
	}

	enriched, _ := json.Marshal(all)
	return enriched
}

func buildSummary(cfg Config) Summary {
	var positions []Position
	posData := fetchPositionsAll(cfg)
	json.Unmarshal(posData, &positions)

	var s Summary
	s.TotalPositions = len(positions)

	for _, p := range positions {
		s.TotalInvested += p.InitialVal
		s.CurrentValue += p.CurrentVal
		s.UnrealizedPnl += p.CashPnl
		s.RealizedPnl += p.RealizedPnl
		s.AvgEntry += p.AvgPrice

		if p.CurPrice >= 0.99 || (p.Redeemable && p.CashPnl > 0) {
			s.WinCount++
		} else if p.CurPrice <= 0.01 || (p.Redeemable && p.CashPnl <= 0) {
			s.LossCount++
		} else {
			s.PendingCount++
		}
	}
	if len(positions) > 0 {
		s.AvgEntry /= float64(len(positions))
	}
	s.TotalPnl = s.UnrealizedPnl + s.RealizedPnl
	closed := s.WinCount + s.LossCount
	if closed > 0 {
		s.WinRate = float64(s.WinCount) / float64(closed) * 100
	}

	// Get trade count.
	tradeData := fetchTrades(cfg)
	var trades []Trade
	json.Unmarshal(tradeData, &trades)
	s.TotalTrades = len(trades)

	// Get balance.
	bal := fetchBalanceCombined(cfg)
	s.UsdcBalance = bal.USDCe + bal.USDC + bal.PUSD

	return s
}

func fetchBalance(address string) Balance {
	rpc := "https://polygon-bor-rpc.publicnode.com"

	callRPC := func(to, data string) string {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_call","params":[{"to":"%s","data":"%s"},"latest"],"id":1}`, to, data)
		resp, err := http.Post(rpc, "application/json", strings.NewReader(body))
		if err != nil {
			return "0x0"
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var r struct {
			Result string `json:"result"`
		}
		json.Unmarshal(b, &r)
		return r.Result
	}

	pad := "000000000000000000000000" + strings.ToLower(address[2:])

	var bal Balance
	// ERC-20 stablecoins have 6 decimals.
	bal.USDC = hexToUnits(callRPC("0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359", "0x70a08231"+pad), 6)
	bal.USDCe = hexToUnits(callRPC("0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174", "0x70a08231"+pad), 6)
	// pUSD lives at the V2 proxy, not the EOA — but read it here for callers
	// that pass the proxy address directly. fetchBalanceCombined merges both.
	bal.PUSD = hexToUnits(callRPC("0xC011a7E12a19f7B1f670d46F03B03f3342E82DFB", "0x70a08231"+pad), 6)

	// POL balance (18 decimals, wei-denominated).
	polBody := fmt.Sprintf(`{"jsonrpc":"2.0","method":"eth_getBalance","params":["%s","latest"],"id":1}`, address)
	resp, err := http.Post(rpc, "application/json", strings.NewReader(polBody))
	if err == nil {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var r struct {
			Result string `json:"result"`
		}
		json.Unmarshal(b, &r)
		bal.POL = hexToUnits(r.Result, 18)
	}

	return bal
}

// hexToUnits parses a hex-encoded integer (with or without a 0x prefix) and
// scales it down by 10^decimals. It uses math/big so values that exceed int64
// don't silently overflow — a POL balance is wei-denominated, so anything above
// ~9.2 POL would wrap a 64-bit signed parse and read as 0.
func hexToUnits(hexStr string, decimals int) float64 {
	hexStr = trimHex(hexStr)
	if hexStr == "" || hexStr == "0" {
		return 0
	}
	n, ok := new(big.Int).SetString(hexStr, 16)
	if !ok {
		return 0
	}
	denom := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(n), new(big.Float).SetInt(denom)).Float64()
	return f
}

// fetchBalanceCombined returns USDC.e/USDC/POL from the EOA and pUSD from
// the V2 Polymarket proxy. After the V2 migration, collateral lives on the proxy.
func fetchBalanceCombined(cfg Config) Balance {
	bal := fetchBalance(cfg.Address)
	if cfg.ProxyAddress != "" {
		proxy := fetchBalance(cfg.ProxyAddress)
		bal.PUSD = proxy.PUSD
	}
	return bal
}

func signHMAC(secret, timestamp, method, path string) string {
	secretBytes, err := base64.URLEncoding.DecodeString(secret)
	if err != nil {
		secretBytes, _ = base64.StdEncoding.DecodeString(secret)
	}
	message := timestamp + method + path
	mac := hmac.New(sha256.New, secretBytes)
	mac.Write([]byte(message))
	return base64.URLEncoding.EncodeToString(mac.Sum(nil))
}

func trimHex(s string) string {
	if len(s) > 2 && s[:2] == "0x" {
		s = s[2:]
	}
	// Remove leading zeros.
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	return s
}
