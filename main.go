package main

import (
	"bytes"
	"compress/zlib"
	"crypto/md5"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Akegarasu/blivedm-go/client"
	"github.com/Akegarasu/blivedm-go/message"
	"github.com/Akegarasu/blivedm-go/packet"
	"github.com/andybalholm/brotli"
	"github.com/gorilla/websocket"
)

//go:embed index.html live-bg.jpg config.json
var embeddedFS embed.FS

// DefaultRoomID 默认硬编码 B站直播间号
const DefaultRoomID = 1879007211

// Config 配置结构（对应 config.json）
type Config struct {
	RoomID     int    `json:"room_id"`
	Cookie     string `json:"cookie"`
	ListenAddr string `json:"listen_addr"`
	Debug      bool   `json:"debug"` // true 时打印每条 B站消息的 cmd 和礼物原始包
	Rules      Rules  `json:"rules"`
}

// debugMode 由 config.json 的 "debug": true 或启动参数 -debug 打开
var debugMode bool

func dbg(format string, args ...any) {
	if debugMode {
		log.Printf("🐞 [DEBUG] "+format, args...)
	}
}

// newWebEnterPacket 进房认证包。blivedm-go 自带的 NewEnterPacket 写死 platform="danmuji"，
// 之前能跑通的 Rust 版用的是 platform="web"，这里对齐成 web。
func newWebEnterPacket(uid int, buvid string, roomID int, key string) []byte {
	body, _ := json.Marshal(map[string]any{
		"uid":      uid,
		"roomid":   roomID,
		"protover": 3,
		"platform": "web",
		"type":     2,
		"buvid":    buvid,
		"key":      key,
	})
	pkt := packet.NewPlainPacket(packet.RoomEnter, body)
	return pkt.Build()
}

// Rules 礼物触发规则
type Rules struct {
	BugsPerGift            int      `json:"bugs_per_gift"`
	CaptainLampGifts       []string `json:"captain_lamp_gifts"`
	ShipDurationSec        int      `json:"ship_duration_sec"`
	ShipUltHits            int      `json:"ship_ult_hits"`
	SummonBugGifts         []string `json:"summon_bug_gifts"`
	SingleKillGifts        []string `json:"single_kill_gifts"`
	BlindBoxGifts          []string `json:"blind_box_gifts"`
	BlindBoxCards          []string `json:"blind_box_cards"`
	GuardAlwaysSummonsShip bool     `json:"guard_always_summons_ship"`
	// UnlistedGiftAction 没写进上面任何名单的礼物怎么处理：
	// "summon_bug"（默认，付费礼物按价值召唤虫子：每 1 电池 1 只；免费礼物不触发）| "none"（不处理）
	UnlistedGiftAction string `json:"unlisted_gift_action"`

	// FixedBugGifts 召唤数量固定的礼物（送几个就 x bugs_per_gift 只），默认「人气票」；
	// 其余召唤类礼物和没配置的付费礼物都会召唤随机数量的虫子，并且有概率变成杀虫剂
	FixedBugGifts []string `json:"fixed_bug_gifts"`
	// NewGuardShipSec 新上舰长的守护战舰存在多少秒（默认 1800 = 30 分钟）
	NewGuardShipSec int `json:"new_guard_ship_sec"`
	// AdmiralUltIntervalSec 已停用（提督船不再定时放全屏大招），留着兼容旧 config
	AdmiralUltIntervalSec int `json:"admiral_ult_interval_sec"`
	// AdmiralLampAutoSec 提督每送 1 个灯牌，舰船自动杀虫多少秒（默认 300 = 5 分钟，可叠加）
	AdmiralLampAutoSec int `json:"admiral_lamp_auto_sec"`
	// 战舰被击沉后多久可以复活：冷却过后主人送灯牌才满血复活
	AdmiralReviveSec int `json:"admiral_revive_sec"` // 提督至尊舰船，默认 300 = 5 分钟
	CaptainReviveSec int `json:"captain_revive_sec"` // 舰长的船，默认 600 = 10 分钟
	// 杀虫剂概率：免费礼物固定 pesticide_free_chance；付费礼物 = base + 单价(元) x per_yuan，最多 max
	PesticideFreeChance float64 `json:"pesticide_free_chance"`
	PesticideBaseChance float64 `json:"pesticide_base_chance"`
	PesticidePerYuan    float64 `json:"pesticide_chance_per_yuan"`
	PesticideMaxChance  float64 `json:"pesticide_max_chance"`
}

// GameEvent 推送给前端透明画布的事件协议
type GameEvent struct {
	Type        string `json:"type"`                   // summon_bug | kill_single | ship_lamp | blind_box
	Count       int    `json:"count,omitempty"`        // 数量（虫子数/杀灭数/灯牌攻击次数）
	User        string `json:"user,omitempty"`         // 观众昵称
	UID         int    `json:"uid,omitempty"`          // 观众 UID
	Avatar      string `json:"avatar,omitempty"`       // 代理后的头像 URL
	Gift        string `json:"gift,omitempty"`         // 礼物名称
	Card        string `json:"card,omitempty"`         // 心动盲盒抽出的纸条内容
	ShipClass   string `json:"ship_class,omitempty"`   // admiral_titan | admiral_dragon | 空(从20艘常规船中轮换)
	DurationSec int    `json:"duration_sec,omitempty"` // 战舰存活秒数（默认60秒）
	UltHits     int    `json:"ult_hits,omitempty"`     // 累计多少个灯牌触发一次随机大招（默认5）
	// Mode 战舰类型：lamp(老舰长灯牌，1分钟) | guard(新上舰长，30分钟自动守护) | admiral(提督至尊舰船，本场常驻)
	Mode           string `json:"mode,omitempty"`
	Power          int    `json:"power,omitempty"`            // 杀虫剂威力：消灭场上百分之几的虫子
	UltIntervalSec int    `json:"ult_interval_sec,omitempty"` // 提督舰船自动大招间隔
	Restore        bool   `json:"restore,omitempty"`          // 画布重连时恢复的提督舰船
	Batteries      int    `json:"batteries,omitempty"`        // 这次礼物值多少电池（没虫子时船主人开炮打别人的船，1 电池 = 1 点血）
	RespawnSec     int    `json:"respawn_sec,omitempty"`      // 战舰被击沉后多少秒才能送灯牌复活
	AutoFireSec    int    `json:"auto_fire_sec,omitempty"`    // 提督船从现在起还要自动杀虫多少秒
	ExpireAt       int64  `json:"expire_at,omitempty"`        // 提督船消失的时间（北京时间当天 24 点，unix 毫秒）
}

// 读完 config 后设置，所有战舰事件统一带上
var (
	admiralReviveSec = 300
	captainReviveSec = 600
)

// withShipRules 给战舰事件补上复活冷却：提督船和舰长船不一样
func withShipRules(ev GameEvent) GameEvent {
	if ev.Type == "ship_lamp" && ev.RespawnSec == 0 {
		if ev.Mode == "admiral" {
			ev.RespawnSec = admiralReviveSec
		} else {
			ev.RespawnSec = captainReviveSec
		}
	}
	return ev
}

// giftBatteries 礼物值多少电池（100 金瓜子 = 1 电池，免费礼物 0）
func giftBatteries(g *message.Gift, num int) int {
	if isFreeGift(g) {
		return 0
	}
	return g.Price * num / 100
}

// admiralRegistry 记录当天（北京时间）召唤过的提督至尊舰船；
// OBS 画布刷新/重连、程序当天重启都会补发回去，过了 24 点自动清空。
// 存在 config.json 旁边的 admirals_today.json 里
type admiralEntry struct {
	Ev        GameEvent `json:"ev"`
	Day       string    `json:"day"`        // 北京时间日期 2006-01-02
	AutoUntil int64     `json:"auto_until"` // 自动杀虫截止时间 unix 毫秒
}

type admiralRegistry struct {
	mu    sync.Mutex
	ships map[string]*admiralEntry
	order []string
	file  string
}

var admirals = &admiralRegistry{ships: map[string]*admiralEntry{}}

var beijing = time.FixedZone("CST", 8*3600)

func beijingDay(t time.Time) string { return t.In(beijing).Format("2006-01-02") }

// beijingMidnight 北京时间下一个 0 点（unix 毫秒）
func beijingMidnight(t time.Time) int64 {
	bt := t.In(beijing)
	return time.Date(bt.Year(), bt.Month(), bt.Day()+1, 0, 0, 0, 0, beijing).UnixMilli()
}

func admiralKey(uid int, user string) string {
	if uid > 0 {
		return "uid:" + strconv.Itoa(uid)
	}
	return "name:" + user
}

// load 程序启动时读回当天的提督船
func (r *admiralRegistry) load(file string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.file = file
	raw, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var saved struct {
		Order []string                 `json:"order"`
		Ships map[string]*admiralEntry `json:"ships"`
	}
	if json.Unmarshal(raw, &saved) != nil || saved.Ships == nil {
		return
	}
	r.ships, r.order = saved.Ships, saved.Order
	r.pruneLocked(time.Now())
	if len(r.order) > 0 {
		log.Printf("[提督] 读回今天的提督舰船 %d 艘（%s）", len(r.order), file)
	}
}

func (r *admiralRegistry) saveLocked() {
	if r.file == "" {
		return
	}
	raw, err := json.MarshalIndent(map[string]any{"order": r.order, "ships": r.ships}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(r.file, raw, 0644); err != nil {
		log.Printf("⚠️ [提督] 保存 %s 失败: %v", r.file, err)
	}
}

// pruneLocked 删掉不是今天的提督船
func (r *admiralRegistry) pruneLocked(now time.Time) {
	today := beijingDay(now)
	keep := r.order[:0]
	changed := false
	for _, k := range r.order {
		if e, ok := r.ships[k]; ok && e.Day == today {
			keep = append(keep, k)
		} else {
			delete(r.ships, k)
			changed = true
		}
	}
	r.order = keep
	if changed {
		r.saveLocked()
	}
}

// fill 给事件补上自动杀虫剩余秒数和当天结束时间
func (e *admiralEntry) fill(ev GameEvent, now time.Time) GameEvent {
	left := e.AutoUntil - now.UnixMilli()
	if left > 0 {
		ev.AutoFireSec = int((left + 999) / 1000)
	} else {
		ev.AutoFireSec = 0
	}
	ev.ExpireAt = beijingMidnight(now)
	return ev
}

// register 登记提督舰船；同一个提督当天始终是同一艘船。lamps 个灯牌，每个加 autoSec 秒自动杀虫（叠加）
func (r *admiralRegistry) register(ev GameEvent, lamps, autoSec int) GameEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.pruneLocked(now)
	key := admiralKey(ev.UID, ev.User)
	e, ok := r.ships[key]
	if ok {
		ev.ShipClass = e.Ev.ShipClass
		if ev.Avatar == "" {
			ev.Avatar = e.Ev.Avatar
		}
	} else {
		e = &admiralEntry{Day: beijingDay(now)}
		r.ships[key] = e
		r.order = append(r.order, key)
	}
	if ev.ShipClass == "" {
		ev.ShipClass = pickAdmiralShipClass()
	}
	if lamps > 0 && autoSec > 0 {
		base := now.UnixMilli()
		if e.AutoUntil > base {
			base = e.AutoUntil
		}
		e.AutoUntil = base + int64(lamps*autoSec)*1000
	}
	saved := ev
	saved.Count = 0
	saved.Batteries = 0
	e.Ev = saved
	r.saveLocked()
	return e.fill(ev, now)
}

func (r *admiralRegistry) list() []GameEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.pruneLocked(now)
	out := make([]GameEvent, 0, len(r.order))
	for _, k := range r.order {
		e := r.ships[k]
		ev := e.fill(e.Ev, now)
		ev.Restore = true
		out = append(out, ev)
	}
	return out
}

// admiralEvent 提督舰船事件；lamps = 这次送了几个灯牌（上舰时为 0）
func admiralEvent(rules *Rules, uid int, user, avatar, gift string, count, lamps int) GameEvent {
	return admirals.register(GameEvent{
		Type: "ship_lamp", Mode: "admiral", Count: count, User: user, UID: uid,
		Avatar: avatar, Gift: gift, UltHits: rules.ShipUltHits,
	}, lamps, rules.AdmiralLampAutoSec)
}

func guardShipEvent(rules *Rules, uid int, user, gift string) GameEvent {
	return GameEvent{
		Type: "ship_lamp", Mode: "guard", Count: 3, User: user, UID: uid, Gift: gift,
		DurationSec: rules.NewGuardShipSec, UltHits: rules.ShipUltHits,
	}
}

// WSHub 管理所有连接到后端的 OBS/浏览器画布
type WSHub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]struct{}
	// writeMu 串行化所有写操作：gorilla/websocket 不允许多个 goroutine 同时写同一连接，
	// 并发写会直接 panic 导致整个程序闪退
	writeMu sync.Mutex
}

func NewWSHub() *WSHub {
	return &WSHub{
		clients: make(map[*websocket.Conn]struct{}),
	}
}

func (h *WSHub) Add(conn *websocket.Conn) {
	h.mu.Lock()
	h.clients[conn] = struct{}{}
	h.mu.Unlock()
	log.Printf("[WS] 画布已连接，当前在线画布数: %d", h.Count())
}

func (h *WSHub) Remove(conn *websocket.Conn) {
	h.mu.Lock()
	if _, ok := h.clients[conn]; ok {
		delete(h.clients, conn)
		_ = conn.Close()
	}
	h.mu.Unlock()
	log.Printf("[WS] 画布已断开，当前在线画布数: %d", h.Count())
}

func (h *WSHub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// SendTo 只发给某一个画布（重连时恢复提督舰船用）
func (h *WSHub) SendTo(conn *websocket.Conn, ev GameEvent) {
	payload, err := json.Marshal(withShipRules(ev))
	if err != nil {
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_ = conn.WriteMessage(websocket.TextMessage, payload)
}

func (h *WSHub) Broadcast(ev GameEvent) {
	payload, err := json.Marshal(withShipRules(ev))
	if err != nil {
		return
	}
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.RUnlock()

	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	for _, c := range conns {
		_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, payload); err != nil {
			h.Remove(c)
		}
	}
}

// AvatarCache 内存缓存，避免重复拉取同一观众头像并解决 B站防盗链/CORS
type cachedAvatar struct {
	contentType string
	data        []byte
	expireAt    time.Time
}

var (
	avatarMu    sync.RWMutex
	avatarCache = make(map[string]cachedAvatar)
	httpClient  = &http.Client{Timeout: 8 * time.Second}
	upgrader    = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
)

func loadOrCreateConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			defRaw, embErr := embeddedFS.ReadFile("config.json")
			if embErr != nil {
				return nil, err
			}
			_ = os.WriteFile(path, defRaw, 0644)
			log.Printf("[配置] 未找到 %s，已自动生成默认配置文件，请填写 room_id 后重启！", path)
			raw = defRaw
		} else {
			return nil, err
		}
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.RoomID <= 0 {
		cfg.RoomID = DefaultRoomID
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.Rules.BugsPerGift <= 0 {
		cfg.Rules.BugsPerGift = 2
	}
	if cfg.Rules.ShipDurationSec <= 0 {
		cfg.Rules.ShipDurationSec = 60
	}
	if cfg.Rules.ShipUltHits <= 0 {
		cfg.Rules.ShipUltHits = 5
	}
	if len(cfg.Rules.CaptainLampGifts) == 0 {
		cfg.Rules.CaptainLampGifts = []string{"粉丝团灯牌", "粉丝灯牌"}
	}
	if cfg.Rules.FixedBugGifts == nil {
		cfg.Rules.FixedBugGifts = []string{"人气票"}
	}
	if cfg.Rules.NewGuardShipSec <= 0 {
		cfg.Rules.NewGuardShipSec = 1800
	}
	if cfg.Rules.AdmiralLampAutoSec <= 0 {
		cfg.Rules.AdmiralLampAutoSec = 300
	}
	if cfg.Rules.AdmiralReviveSec <= 0 {
		cfg.Rules.AdmiralReviveSec = 300
	}
	if cfg.Rules.CaptainReviveSec <= 0 {
		cfg.Rules.CaptainReviveSec = 600
	}
	admiralReviveSec = cfg.Rules.AdmiralReviveSec
	captainReviveSec = cfg.Rules.CaptainReviveSec
	if cfg.Rules.PesticideFreeChance <= 0 {
		cfg.Rules.PesticideFreeChance = 0.02
	}
	if cfg.Rules.PesticideBaseChance <= 0 {
		cfg.Rules.PesticideBaseChance = 0.05
	}
	if cfg.Rules.PesticidePerYuan <= 0 {
		cfg.Rules.PesticidePerYuan = 0.02
	}
	if cfg.Rules.PesticideMaxChance <= 0 {
		cfg.Rules.PesticideMaxChance = 0.5
	}
	return &cfg, nil
}

func containsGift(list []string, giftName string) bool {
	target := strings.TrimSpace(giftName)
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}

func pickBlindBoxCard(cards []string) string {
	if len(cards) == 0 {
		return "万能指定卡(其他任何1个)"
	}
	return cards[rand.Intn(len(cards))]
}

func pickAdmiralShipClass() string {
	if rand.Intn(2) == 0 {
		return "admiral_titan"
	}
	return "admiral_dragon"
}

func proxyAvatarURL(rawFace string) string {
	if rawFace == "" {
		return ""
	}
	return "/avatar?u=" + url.QueryEscape(rawFace)
}

func handleAvatarProxy(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("u")
	if rawURL == "" {
		http.Error(w, "missing u", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	host := strings.ToLower(parsed.Hostname())
	if !strings.HasSuffix(host, "hdslb.com") && !strings.HasSuffix(host, "bilibili.com") {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	avatarMu.RLock()
	cached, ok := avatarCache[rawURL]
	avatarMu.RUnlock()
	if ok && time.Now().Before(cached.expireAt) {
		w.Header().Set("Content-Type", cached.contentType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(cached.data)
		return
	}

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Referer", "https://live.bilibili.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")

	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadGateway)
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}

	avatarMu.Lock()
	if len(avatarCache) > 300 {
		avatarCache = make(map[string]cachedAvatar)
	}
	avatarCache[rawURL] = cachedAvatar{
		contentType: ct,
		data:        data,
		expireAt:    time.Now().Add(30 * time.Minute),
	}
	avatarMu.Unlock()

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_, _ = w.Write(data)
}

func guardLevelName(level int) string {
	switch level {
	case 1:
		return "总督"
	case 2:
		return "提督"
	case 3:
		return "舰长"
	default:
		return "普通"
	}
}

const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36"

var wbiMixinTab = [64]int{
	46, 47, 18, 2, 53, 8, 23, 32,
	15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19,
	29, 28, 14, 39, 12, 38, 41, 13,
	37, 48, 7, 16, 24, 55, 40, 61,
	26, 17, 0, 1, 60, 51, 30, 4,
	22, 25, 54, 21, 56, 59, 6, 63,
	57, 62, 11, 36, 20, 34, 44, 52,
}

func fetchWbiAndUID(cookie string) (mixinKey string, uid int, err error) {
	req, err := http.NewRequest("GET", "https://api.bilibili.com/x/web-interface/nav", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Referer", "https://live.bilibili.com/")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	var nav struct {
		Code int `json:"code"`
		Data struct {
			IsLogin bool `json:"isLogin"`
			Mid     int  `json:"mid"`
			WbiImg  struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nav); err != nil {
		return "", 0, err
	}
	if nav.Data.WbiImg.ImgURL == "" || nav.Data.WbiImg.SubURL == "" {
		return "", 0, fmt.Errorf("未获取到 wbi_img (code=%d)", nav.Code)
	}
	extractKey := func(u string) string {
		parts := strings.Split(u, "/")
		return strings.TrimSuffix(parts[len(parts)-1], ".png")
	}
	rawKey := extractKey(nav.Data.WbiImg.ImgURL) + extractKey(nav.Data.WbiImg.SubURL)
	var mixin [32]byte
	for i := 0; i < 32 && i < len(wbiMixinTab); i++ {
		if wbiMixinTab[i] < len(rawKey) {
			mixin[i] = rawKey[wbiMixinTab[i]]
		}
	}
	if nav.Data.IsLogin {
		uid = nav.Data.Mid
	}
	return string(mixin[:]), uid, nil
}

func fetchGuestBuvid3() string {
	req, err := http.NewRequest("GET", "https://api.bilibili.com/x/frontend/finger/spi", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Referer", "https://live.bilibili.com/")
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var res struct {
		Data struct {
			B3 string `json:"b_3"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err == nil {
		return res.Data.B3
	}
	return ""
}

func extractCookieField(cookie, key string) string {
	re := regexp.MustCompile(key + `=([^;]+)`)
	m := re.FindStringSubmatch(cookie)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func fetchRealRoomID(roomID int) int {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://api.live.bilibili.com/room/v1/Room/room_init?id=%d", roomID), nil)
	if err != nil {
		return roomID
	}
	req.Header.Set("User-Agent", chromeUA)
	resp, err := httpClient.Do(req)
	if err != nil {
		return roomID
	}
	defer resp.Body.Close()
	var res struct {
		Code int `json:"code"`
		Data struct {
			RoomID int `json:"room_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err == nil && res.Code == 0 && res.Data.RoomID > 0 {
		return res.Data.RoomID
	}
	return roomID
}

// anchorUID 当前直播间主播的 UID，用来判断观众戴的粉丝牌是不是本直播间的
var anchorUID atomic.Int64

type danmuConnInfo struct {
	RealRoomID int
	UID        int
	Buvid3     string
	Token      string
	Hosts      []string
}

func fetchDanmuConnInfo(roomID int, cookie string) (*danmuConnInfo, error) {
	jar, _ := cookiejar.New(nil)
	cli := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
	}

	doGet := func(rawURL string, extraHeaders map[string]string) ([]byte, error) {
		req, err := http.NewRequest("GET", rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", chromeUA)
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}
		resp, err := cli.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}

	// 1. 先访问 www.bilibili.com，通过 CookieJar 自动拿合法的 buvid3（匿名免登录核心步骤！）
	if _, err := doGet("https://www.bilibili.com/", nil); err != nil {
		log.Printf("⚠️ [B站] 访问 www.bilibili.com 拿 buvid3 失败: %v", err)
	}
	buvid3 := extractCookieField(cookie, "buvid3")
	if buvid3 == "" {
		baseURL, _ := url.Parse("https://www.bilibili.com/")
		for _, ck := range jar.Cookies(baseURL) {
			if ck.Name == "buvid3" {
				buvid3 = ck.Value
				break
			}
		}
	}

	// 2. 获取 nav（WBI 密钥 + 登录 UID）
	navBody, err := doGet("https://api.bilibili.com/x/web-interface/nav", map[string]string{
		"Referer": "https://live.bilibili.com/",
	})
	if err != nil {
		return nil, fmt.Errorf("请求 nav 失败: %w", err)
	}
	var nav struct {
		Code int `json:"code"`
		Data struct {
			IsLogin bool `json:"isLogin"`
			Mid     int  `json:"mid"`
			WbiImg  struct {
				ImgURL string `json:"img_url"`
				SubURL string `json:"sub_url"`
			} `json:"wbi_img"`
		} `json:"data"`
	}
	if err := json.Unmarshal(navBody, &nav); err != nil {
		return nil, err
	}
	if nav.Data.WbiImg.ImgURL == "" || nav.Data.WbiImg.SubURL == "" {
		return nil, fmt.Errorf("未获取到 wbi_img (code=%d)", nav.Code)
	}
	extractKey := func(u string) string {
		parts := strings.Split(u, "/")
		return strings.TrimSuffix(parts[len(parts)-1], ".png")
	}
	rawKey := extractKey(nav.Data.WbiImg.ImgURL) + extractKey(nav.Data.WbiImg.SubURL)
	var mixin [32]byte
	for i := 0; i < 32 && i < len(wbiMixinTab); i++ {
		if wbiMixinTab[i] < len(rawKey) {
			mixin[i] = rawKey[wbiMixinTab[i]]
		}
	}
	mixinKey := string(mixin[:])
	uid := 0
	if nav.Data.IsLogin {
		uid = nav.Data.Mid
	}

	// 3. 获取真实房间号
	realRoomID := roomID
	if rBody, err := doGet(fmt.Sprintf("https://api.live.bilibili.com/room/v1/Room/get_info?room_id=%d", roomID), nil); err == nil {
		var rInfo struct {
			Code int `json:"code"`
			Data struct {
				RoomID int `json:"room_id"`
				UID    int `json:"uid"`
			} `json:"data"`
		}
		if json.Unmarshal(rBody, &rInfo) == nil && rInfo.Code == 0 && rInfo.Data.RoomID > 0 {
			realRoomID = rInfo.Data.RoomID
			if rInfo.Data.UID > 0 {
				anchorUID.Store(int64(rInfo.Data.UID))
			}
		}
	}

	// 4. WBI 签名请求 getDanmuInfo 拿弹幕服务器与 Token
	vals := url.Values{}
	vals.Set("id", strconv.Itoa(realRoomID))
	vals.Set("type", "0")
	vals.Set("wts", strconv.FormatInt(time.Now().Unix(), 10))
	query := vals.Encode()
	sum := md5.Sum([]byte(query + mixinKey))
	signedURL := "https://api.live.bilibili.com/xlive/web-room/v1/index/getDanmuInfo?" + query + "&w_rid=" + hex.EncodeToString(sum[:])

	dmBody, err := doGet(signedURL, map[string]string{
		"Origin":  "https://live.bilibili.com",
		"Referer": fmt.Sprintf("https://live.bilibili.com/%d", realRoomID),
	})
	if err != nil {
		return nil, err
	}

	var info struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Token    string `json:"token"`
			HostList []struct {
				Host string `json:"host"`
			} `json:"host_list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(dmBody, &info); err != nil {
		return nil, err
	}
	if info.Code != 0 || info.Data.Token == "" {
		return nil, fmt.Errorf("getDanmuInfo 返回错误 code=%d msg=%s", info.Code, info.Message)
	}

	hosts := make([]string, 0, len(info.Data.HostList))
	for _, h := range info.Data.HostList {
		if h.Host != "" {
			hosts = append(hosts, h.Host)
		}
	}
	if len(hosts) == 0 {
		hosts = []string{"broadcastlv.chat.bilibili.com"}
	}

	log.Printf("ℹ️ [B站] 连接信息: 配置房间=%d 真实房间=%d 登录=%v UID=%d buvid3=%s token长度=%d 服务器=%v",
		roomID, realRoomID, nav.Data.IsLogin, uid, maskStr(buvid3), len(info.Data.Token), hosts)
	if buvid3 == "" {
		log.Printf("⚠️ [B站] buvid3 为空！匿名连接没有 buvid3 很可能收不到礼物/弹幕")
	}

	return &danmuConnInfo{
		RealRoomID: realRoomID,
		UID:        uid,
		Buvid3:     buvid3,
		Token:      info.Data.Token,
		Hosts:      hosts,
	}, nil
}

// 解析 SEND_GIFT_V2 protobuf（对应 bili.rs 里的 SendGiftBroadcast / GiftItem）
func readProtoVarint(b []byte, pos int) (uint64, int, bool) {
	var v uint64
	var shift uint
	for pos < len(b) && shift < 64 {
		c := b[pos]
		pos++
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, pos, true
		}
		shift += 7
	}
	return 0, pos, false
}

// walkPB 逐个遍历 protobuf 字段：varint 字段给 val，length-delimited 字段给 chunk
func walkPB(b []byte, fn func(field uint64, val uint64, chunk []byte, isBytes bool)) {
	for pos := 0; pos < len(b); {
		tag, next, ok := readProtoVarint(b, pos)
		if !ok {
			return
		}
		pos = next
		fieldNum := tag >> 3
		switch tag & 7 {
		case 0:
			val, n2, ok2 := readProtoVarint(b, pos)
			if !ok2 {
				return
			}
			pos = n2
			fn(fieldNum, val, nil, false)
		case 2:
			ln, n2, ok2 := readProtoVarint(b, pos)
			if !ok2 || n2+int(ln) > len(b) {
				return
			}
			fn(fieldNum, 0, b[n2:n2+int(ln)], true)
			pos = n2 + int(ln)
		case 1:
			pos += 8
		case 5:
			pos += 4
		default:
			return
		}
	}
}

// 字段编号对照 blivedm/models/pb.py 的 SendGiftBroadcast / SendGiftV2GiftItem / BlindGift / MedalInfo
func decodeSendGiftV2PB(pbB64 string) []*message.Gift {
	raw, err := base64.StdEncoding.DecodeString(pbB64)
	if err != nil {
		return nil
	}
	var uid, guardLevel, medalTarget int
	var uname, face, origGiftName string
	var items []*message.Gift

	walkPB(raw, func(f uint64, v uint64, chunk []byte, isBytes bool) {
		switch {
		case f == 1 && !isBytes:
			uid = int(v)
		case f == 2 && isBytes:
			uname = string(chunk)
		case f == 3 && isBytes:
			face = string(chunk)
		case f == 5 && !isBytes:
			guardLevel = int(v)
		case f == 8 && isBytes: // medal_info
			walkPB(chunk, func(mf uint64, mv uint64, _ []byte, mb bool) {
				if mf == 1 && !mb {
					medalTarget = int(mv)
				}
			})
		case f == 9 && isBytes: // blind_gift
			walkPB(chunk, func(bf uint64, _ uint64, bc []byte, bb bool) {
				if bf == 3 && bb {
					origGiftName = string(bc)
				}
			})
		case f == 10 && isBytes: // gift_list 每一项
			it := &message.Gift{Num: 1}
			walkPB(chunk, func(gf uint64, gv uint64, gc []byte, gb bool) {
				switch {
				case gf == 2 && gb:
					it.GiftName = string(gc)
				case gf == 3 && !gb:
					it.Num = int(gv)
				case gf == 5 && !gb:
					it.Price = int(gv)
				case gf == 7 && !gb:
					it.TotalCoin = int(gv)
				case gf == 8 && gb:
					it.CoinType = string(gc)
				case gf == 9 && gb:
					it.Tid = string(gc)
				}
			})
			if it.GiftName != "" {
				items = append(items, it)
			}
		}
	})

	for _, it := range items {
		it.Uid = uid
		it.Uname = uname
		it.Face = face
		it.GuardLevel = guardLevel
		it.OriginalGiftName = origGiftName
		it.MedalInfo.TargetId = medalTarget
	}
	return items
}

func parseSendGiftJSON(body []byte) (*message.Gift, bool) {
	var pkt struct {
		Data struct {
			UID        int    `json:"uid"`
			Uname      string `json:"uname"`
			Face       string `json:"face"`
			GiftName   string `json:"giftName"`
			Num        int    `json:"num"`
			GuardLevel int    `json:"guard_level"`
			Price      int    `json:"price"`
			TotalCoin  int    `json:"total_coin"`
			CoinType   string `json:"coin_type"`
			Tid        string `json:"tid"`
			MedalInfo  struct {
				GuardLevel int `json:"guard_level"`
				TargetID   int `json:"target_id"`
			} `json:"medal_info"`
			BlindGift struct {
				OriginalGiftName string `json:"original_gift_name"`
			} `json:"blind_gift"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &pkt); err != nil || pkt.Data.GiftName == "" {
		return nil, false
	}
	g := &message.Gift{
		Uid:              pkt.Data.UID,
		Uname:            pkt.Data.Uname,
		Face:             pkt.Data.Face,
		GiftName:         pkt.Data.GiftName,
		OriginalGiftName: pkt.Data.BlindGift.OriginalGiftName,
		Num:              pkt.Data.Num,
		GuardLevel:       pkt.Data.GuardLevel,
		Price:            pkt.Data.Price,
		TotalCoin:        pkt.Data.TotalCoin,
		CoinType:         pkt.Data.CoinType,
		Tid:              pkt.Data.Tid,
	}
	g.MedalInfo.GuardLevel = pkt.Data.MedalInfo.GuardLevel
	g.MedalInfo.TargetId = pkt.Data.MedalInfo.TargetID
	return g, true
}

// parseBiliPackets 完全按照 bili.rs 的递归拆包逻辑，支持单帧多包拼接 + 嵌套 brotli/zlib 压缩
func parseBiliPackets(data []byte, out *[]packet.Packet) {
	off := 0
	for off+16 <= len(data) {
		packLen := int(binary.BigEndian.Uint32(data[off : off+4]))
		headLen := int(binary.BigEndian.Uint16(data[off+4 : off+6]))
		protoVer := binary.BigEndian.Uint16(data[off+6 : off+8])
		op := binary.BigEndian.Uint32(data[off+8 : off+12])

		if packLen < 16 || headLen > packLen || off+packLen > len(data) {
			break
		}
		body := data[off+headLen : off+packLen]

		if op == 5 && protoVer == 3 {
			// Brotli 嵌套压缩包
			if dec, err := io.ReadAll(brotli.NewReader(bytes.NewReader(body))); err == nil {
				parseBiliPackets(dec, out)
			} else {
				log.Printf("⚠️ [B站] brotli 解压失败: %v (包长 %d)", err, len(body))
			}
		} else if op == 5 && protoVer == 2 {
			// Zlib 嵌套压缩包
			if zr, err := zlib.NewReader(bytes.NewReader(body)); err == nil {
				if dec, err := io.ReadAll(zr); err == nil {
					parseBiliPackets(dec, out)
				} else {
					log.Printf("⚠️ [B站] zlib 解压失败: %v", err)
				}
				_ = zr.Close()
			} else {
				log.Printf("⚠️ [B站] zlib 打开失败: %v", err)
			}
		} else {
			cp := make([]byte, len(body))
			copy(cp, body)
			*out = append(*out, packet.Packet{
				ProtocolVersion: protoVer,
				Operation:       op,
				Body:            cp,
			})
		}
		off += packLen
	}
	if off < len(data) {
		dbg("拆包剩余 %d 字节未解析（总长 %d）", len(data)-off, len(data))
	}
}

func maskStr(s string) string {
	if s == "" {
		return "(空)"
	}
	if len(s) <= 8 {
		return s[:2] + "***"
	}
	return s[:4] + "***" + s[len(s)-4:]
}

// cmdStats 统计每条连接收到的各类 cmd 数量，定时打印，用来判断"B站到底有没有推礼物过来"
type cmdStats struct {
	mu     sync.Mutex
	counts map[string]int
}

func (s *cmdStats) add(cmd string) {
	s.mu.Lock()
	s.counts[cmd]++
	s.mu.Unlock()
}

func (s *cmdStats) flush() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.counts) == 0 {
		return "（什么都没收到）"
	}
	parts := make([]string, 0, len(s.counts))
	for k, v := range s.counts {
		parts = append(parts, fmt.Sprintf("%s×%d", k, v))
	}
	s.counts = make(map[string]int)
	return strings.Join(parts, ", ")
}

// giftDeduper 防止同一个礼物被 SEND_GIFT 和 SEND_GIFT_V2 各算一次。
// 规则：一条礼物到来时，如果 5 秒内「另一种来源」已经有一条 uid+礼物名+数量 完全相同、且还没被配对的记录，
// 就认为是同一个礼物的重复推送，丢弃并消耗掉那条记录；否则正常处理并记下来。
// 连击时同一来源会连续来多条相同礼物，它们之间不会互相抵消，所以不会误吞真实连击。
type giftDeduper struct {
	mu      sync.Mutex
	pending map[string][]giftSeen
}

type giftSeen struct {
	src string
	at  time.Time
}

const giftDedupWindow = 5 * time.Second

func newGiftDeduper() *giftDeduper {
	return &giftDeduper{pending: make(map[string][]giftSeen)}
}

// accept 返回 true 表示这条礼物应当处理
func (d *giftDeduper) accept(src string, g *message.Gift) bool {
	// 有 tid（每笔送礼唯一编号，新旧两种消息里相同）就用 tid 精确去重；没有再退回 用户+礼物+数量
	key := "tid:" + g.Tid
	if g.Tid == "" {
		key = fmt.Sprintf("%d|%s|%d", g.Uid, strings.TrimSpace(g.GiftName), g.Num)
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()

	// 清理过期记录
	for k, list := range d.pending {
		kept := list[:0]
		for _, it := range list {
			if now.Sub(it.at) < giftDedupWindow {
				kept = append(kept, it)
			}
		}
		if len(kept) == 0 {
			delete(d.pending, k)
		} else {
			d.pending[k] = kept
		}
	}

	list := d.pending[key]
	for i, it := range list {
		if it.src != src {
			d.pending[key] = append(list[:i], list[i+1:]...)
			return false
		}
	}
	d.pending[key] = append(list, giftSeen{src: src, at: now})
	return true
}

func runBilibiliWSLoop(c *client.Client, roomID int, cookie string, onGiftFn func(*message.Gift), stopCh <-chan struct{}) {
	retry := 0

	// 所有礼物进同一条队列、由一个 worker 按到达顺序依次处理，
	// 既不会阻塞 B站读包循环，也保证同一个舰长连刷灯牌时攻击/充能顺序正确
	dedup := newGiftDeduper()
	giftCh := make(chan *message.Gift, 1024)
	go func() {
		for {
			select {
			case <-stopCh:
				return
			case g := <-giftCh:
				onGiftFn(g)
			}
		}
	}()
	dispatchGift := func(src string, g *message.Gift) {
		if !dedup.accept(src, g) {
			log.Printf("♻️ [B站礼物] 跳过重复推送(%s): %s 送出「%s」x%d", src, g.Uname, g.GiftName, g.Num)
			return
		}
		select {
		case giftCh <- g:
		default:
			log.Printf("⚠️ [B站礼物] 礼物队列已满，丢弃: %s「%s」x%d", g.Uname, g.GiftName, g.Num)
		}
	}
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		info, err := fetchDanmuConnInfo(roomID, cookie)
		if err != nil {
			log.Printf("❌ [B站] 获取弹幕服务器失败: %v（3秒后重试）", err)
			select {
			case <-stopCh:
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}

		host := info.Hosts[retry%len(info.Hosts)]
		retry++

		hdr := http.Header{}
		hdr.Set("User-Agent", chromeUA)
		hdr.Set("Origin", "https://live.bilibili.com")

		conn, _, err := websocket.DefaultDialer.Dial("wss://"+host+"/sub", hdr)
		if err != nil {
			log.Printf("❌ [B站] 连接 wss://%s/sub 失败: %v（3秒后重试）", host, err)
			select {
			case <-stopCh:
				return
			case <-time.After(3 * time.Second):
				continue
			}
		}

		enterPkt := newWebEnterPacket(info.UID, info.Buvid3, info.RealRoomID, info.Token)
		dbg("发送进房包 uid=%d roomid=%d platform=web protover=3 buvid=%s", info.UID, info.RealRoomID, maskStr(info.Buvid3))
		if err := conn.WriteMessage(websocket.BinaryMessage, enterPkt); err != nil {
			_ = conn.Close()
			log.Printf("❌ [B站] 发送进房认证包失败: %v（3秒后重试）", err)
			time.Sleep(3 * time.Second)
			continue
		}

		modeText := "匿名免登录模式"
		if info.UID > 0 {
			modeText = fmt.Sprintf("已登录 UID:%d", info.UID)
		}
		log.Printf("🔗 [B站] 已连接直播间 %d (%s, 服务器: %s)，等待认证回复...", info.RealRoomID, modeText, host)

		hbDone := make(chan struct{})
		var writeMu sync.Mutex
		stats := &cmdStats{counts: make(map[string]int)}
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			statTicker := time.NewTicker(60 * time.Second)
			defer statTicker.Stop()
			hbPkt := packet.NewHeartBeatPacket()
			for {
				select {
				case <-hbDone:
					return
				case <-stopCh:
					return
				case <-ticker.C:
					writeMu.Lock()
					err := conn.WriteMessage(websocket.BinaryMessage, hbPkt)
					writeMu.Unlock()
					if err != nil {
						log.Printf("⚠️ [B站] 心跳发送失败: %v", err)
					} else {
						dbg("心跳已发送")
					}
				case <-statTicker.C:
					// 这行一直打印：看有没有 SEND_GIFT / SEND_GIFT_V2 就知道 B站有没有推礼物
					log.Printf("📊 [B站统计] 最近60秒收到: %s", stats.flush())
				}
			}
		}()

		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				close(hbDone)
				_ = conn.Close()
				select {
				case <-stopCh:
					return
				default:
					log.Printf("⚠️ [B站] 连接断开: %v，3秒后自动重连...", err)
					time.Sleep(3 * time.Second)
				}
				break
			}
			if msgType != websocket.BinaryMessage {
				dbg("收到非二进制消息 type=%d len=%d", msgType, len(data))
				continue
			}
			var pkts []packet.Packet
			parseBiliPackets(data, &pkts)
			if len(pkts) == 0 {
				log.Printf("⚠️ [B站] 收到 %d 字节数据但拆出 0 个包，前32字节: %x", len(data), data[:min(32, len(data))])
			}
			for _, pkt := range pkts {
				// op == 3: 心跳回复（人气值），能收到说明连接是活的
				if pkt.Operation == 3 {
					stats.add("心跳回复")
					dbg("心跳回复 op=3 len=%d", len(pkt.Body))
				}
				// op == 8: 进房认证回复包
				if pkt.Operation == 8 {
					var rep struct {
						Code int `json:"code"`
					}
					if json.Unmarshal(pkt.Body, &rep) == nil && rep.Code == 0 {
						log.Printf("✅ [B站] 进房认证成功 (code=0)！实时监听弹幕与礼物中...")
					} else {
						log.Printf("❌ [B站] 进房认证失败: %s", string(pkt.Body))
					}
					continue
				}
				// op == 5: 业务消息包，优先可靠解析 SEND_GIFT / SEND_GIFT_V2
				if pkt.Operation == 5 {
					var hdrObj struct {
						Cmd string `json:"cmd"`
					}
					if err := json.Unmarshal(pkt.Body, &hdrObj); err != nil || hdrObj.Cmd == "" {
						log.Printf("⚠️ [B站] op=5 包不是合法 JSON 或没有 cmd (err=%v ver=%d): %.200q", err, pkt.ProtocolVersion, pkt.Body)
					}
					if hdrObj.Cmd != "" {
						baseCmd := strings.SplitN(hdrObj.Cmd, ":", 2)[0]
						stats.add(baseCmd)
						dbg("cmd=%s len=%d", hdrObj.Cmd, len(pkt.Body))
						if strings.Contains(baseCmd, "GIFT") || strings.Contains(baseCmd, "COMBO") || strings.Contains(baseCmd, "GUARD") {
							dbg("礼物相关原始包 cmd=%s: %.800s", hdrObj.Cmd, string(pkt.Body))
						}
						switch baseCmd {
						case "SEND_GIFT":
							if g, ok := parseSendGiftJSON(pkt.Body); ok {
								dispatchGift("SEND_GIFT", g)
							} else {
								log.Printf("⚠️ [B站礼物] SEND_GIFT 解析失败，原始包: %.300s", string(pkt.Body))
							}
							continue
						case "SEND_GIFT_V2":
							var v2 struct {
								Data struct {
									PB string `json:"pb"`
								} `json:"data"`
							}
							if json.Unmarshal(pkt.Body, &v2) == nil && v2.Data.PB != "" {
								gifts := decodeSendGiftV2PB(v2.Data.PB)
								if len(gifts) > 0 {
									for _, g := range gifts {
										dispatchGift("SEND_GIFT_V2", g)
									}
								} else {
									log.Printf("⚠️ [B站礼物] SEND_GIFT_V2 Protobuf 解出0条礼物")
								}
							} else {
								log.Printf("⚠️ [B站礼物] SEND_GIFT_V2 缺少 pb 字段: %.200s", string(pkt.Body))
							}
							continue
						default:
							if strings.Contains(baseCmd, "GIFT") || strings.Contains(baseCmd, "COMBO") {
								log.Printf("ℹ️ [B站其他礼物事件] cmd=%s", hdrObj.Cmd)
							}
						}
					}
				}
				go c.Handle(pkt)
			}
		}
	}
}

// resolveGuardLevel 舰长身份只认本直播间：优先用礼物包里的 guard_level；
// 只有当观众戴的粉丝牌确实属于本直播间主播时，才用牌子上的舰长等级兜底
// （否则戴着别家舰长牌的人也能在这里召唤战舰）
func resolveGuardLevel(g *message.Gift) int {
	if g.GuardLevel > 0 {
		return g.GuardLevel
	}
	if g.MedalInfo.GuardLevel > 0 {
		if a := anchorUID.Load(); a > 0 && int64(g.MedalInfo.TargetId) == a {
			return g.MedalInfo.GuardLevel
		}
		dbg("忽略非本直播间粉丝牌的舰长等级: uid=%d medalTarget=%d anchor=%d", g.Uid, g.MedalInfo.TargetId, anchorUID.Load())
	}
	return 0
}

// isFreeGift 免费礼物（银瓜子 / 价格为 0）
func isFreeGift(g *message.Gift) bool {
	return g.CoinType != "gold" || g.Price <= 0
}

// bugsForUnlistedGift 付费礼物按价值召唤虫子：每 1 电池 1 只（100 金瓜子 = 1 电池，不足 1 电池/免费礼物也算 1 只）
func bugsForUnlistedGift(g *message.Gift, num int) int {
	perUnit := 1
	if !isFreeGift(g) && g.Price >= 100 {
		perUnit = g.Price / 100
	}
	return num * perUnit
}

const maxBugsPerEvent = 2000

// pesticideChance 礼物越贵，变成杀虫剂的概率越高
func pesticideChance(rules *Rules, g *message.Gift) float64 {
	if isFreeGift(g) {
		return rules.PesticideFreeChance
	}
	yuan := float64(g.Price) / 1000
	p := rules.PesticideBaseChance + yuan*rules.PesticidePerYuan
	if p > rules.PesticideMaxChance {
		p = rules.PesticideMaxChance
	}
	return p
}

// pesticidePower 杀虫剂威力（消灭场上百分之几的虫子）：免费 20%，付费 35% 起每元 +5%，最多 100%
func pesticidePower(g *message.Gift, num int) int {
	if isFreeGift(g) {
		return 20
	}
	yuan := g.Price * num / 1000
	p := 35 + yuan*5
	if p > 100 {
		p = 100
	}
	return p
}

// randomGiftOutcome 普通礼物：先掷骰子看是不是杀虫剂，不是的话召唤随机数量的虫子（基础值的 0.5~2 倍）
func randomGiftOutcome(rules *Rules, g *message.Gift, num int, base int, role string) []GameEvent {
	if rand.Float64() < pesticideChance(rules, g) {
		power := pesticidePower(g, num)
		log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d -> 🧴 触发杀虫剂！消灭场上 %d%% 的虫子", g.Uname, g.Uid, role, g.GiftName, num, power)
		return []GameEvent{{Type: "pesticide", Power: power, Count: num, User: g.Uname, UID: g.Uid, Gift: g.GiftName}}
	}
	if base < 1 {
		base = 1
	}
	lo := base / 2
	if lo < 1 {
		lo = 1
	}
	hi := base * 2
	count := lo + rand.Intn(hi-lo+1)
	if count > maxBugsPerEvent {
		count = maxBugsPerEvent
	}
	log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d (单价 %d %s) -> 🐛 随机召唤 %d 只虫子", g.Uname, g.Uid, role, g.GiftName, num, g.Price, g.CoinType, count)
	return []GameEvent{{Type: "summon_bug", Count: count, User: g.Uname, UID: g.Uid, Gift: g.GiftName}}
}

// 上舰本身由 GUARD_BUY 事件处理，礼物流里出现的舰长/提督/总督不再当普通礼物
var guardGiftNames = []string{"舰长", "提督", "总督"}

// classifyGift 按 config 规则把一条礼物翻译成要推给画布的游戏事件
func classifyGift(rules *Rules, g *message.Gift) []GameEvent {
	num := g.Num
	if num <= 0 {
		num = 1
	}
	guardLevel := resolveGuardLevel(g)
	role := guardLevelName(guardLevel)

	// 盲盒礼物在 B站下发时 giftName 是开出来的道具名，原盲盒名在 OriginalGiftName 中
	effectiveGift := g.GiftName
	if g.OriginalGiftName != "" && containsGift(rules.BlindBoxGifts, g.OriginalGiftName) {
		effectiveGift = g.OriginalGiftName
	}

	switch {
	// 1. 心动盲盒 -> 送几个开几个（前端排队依次开箱）；只设一个很高的保护上限防止异常数据
	case containsGift(rules.BlindBoxGifts, effectiveGift):
		boxCount := num
		if boxCount > 200 {
			log.Printf("⚠️ [B站礼物] 心动盲盒数量异常 x%d，本次按 200 个处理", num)
			boxCount = 200
		}
		evs := make([]GameEvent, 0, boxCount)
		for i := 0; i < boxCount; i++ {
			card := pickBlindBoxCard(rules.BlindBoxCards)
			log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」(开出「%s」) x%d -> 🎁 心动盲盒第 %d 个，抽出纸条: %s", g.Uname, g.Uid, role, effectiveGift, g.GiftName, num, i+1, card)
			evs = append(evs, GameEvent{Type: "blind_box", User: g.Uname, UID: g.Uid, Gift: effectiveGift, Card: card})
		}
		return evs

	// 2. 粉丝团灯牌 -> 仅本直播间舰长(3)/提督(2)/总督(1)触发战舰与炮击；普通观众走单个杀灭
	case containsGift(rules.CaptainLampGifts, g.GiftName):
		if guardLevel == 1 || guardLevel == 2 {
			ev := admiralEvent(rules, g.Uid, g.Uname, proxyAvatarURL(g.Face), g.GiftName, num, num)
			log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d -> 👑 提督至尊舰船(%s，当天常驻)开火 %d 次，自动杀虫还剩 %d 秒", g.Uname, g.Uid, role, g.GiftName, num, ev.ShipClass, num, ev.AutoFireSec)
			return []GameEvent{ev}
		}
		if guardLevel > 0 {
			log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d -> ⛵ 触发舰长战舰攻击 %d 次", g.Uname, g.Uid, role, g.GiftName, num, num)
			return []GameEvent{{
				Type: "ship_lamp", Mode: "lamp", Count: num, User: g.Uname, UID: g.Uid,
				Avatar: proxyAvatarURL(g.Face), Gift: g.GiftName,
				DurationSec: rules.ShipDurationSec, UltHits: rules.ShipUltHits,
			}}
		}
		log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d -> 🩴 触发单个杀灭 %d 只虫子", g.Uname, g.Uid, role, g.GiftName, num, num)
		return []GameEvent{{Type: "kill_single", Count: num, User: g.Uname, UID: g.Uid, Gift: g.GiftName}}

	// 3. 单个杀灭类礼物
	case containsGift(rules.SingleKillGifts, g.GiftName):
		log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d -> 🩴 触发单个杀灭 %d 只虫子", g.Uname, g.Uid, role, g.GiftName, num, num)
		return []GameEvent{{Type: "kill_single", Count: num, User: g.Uname, UID: g.Uid, Gift: g.GiftName}}

	// 上舰礼物交给 GUARD_BUY 处理
	case containsGift(guardGiftNames, g.GiftName):
		log.Printf("[B站礼物] %s(UID:%d) 礼物流里的「%s」由上舰事件处理，这里跳过", g.Uname, g.Uid, g.GiftName)
		return nil

	// 4a. 人气票等固定数量礼物：送几个 x bugs_per_gift 只，没有上限（画布放不下的前端会合体成大虫）
	case containsGift(rules.FixedBugGifts, g.GiftName):
		count := num * rules.BugsPerGift
		if count > maxBugsPerEvent {
			count = maxBugsPerEvent
		}
		log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d -> 🐛 触发召唤 %d 只虫子", g.Uname, g.Uid, role, g.GiftName, num, count)
		return []GameEvent{{Type: "summon_bug", Count: count, User: g.Uname, UID: g.Uid, Gift: g.GiftName}}

	// 4b. 其他召唤类礼物：随机数量虫子，或者杀虫剂
	case containsGift(rules.SummonBugGifts, g.GiftName):
		return randomGiftOutcome(rules, g, num, bugsForUnlistedGift(g, num)*rules.BugsPerGift, role)

	// 5. 其他没写进 config 的付费礼物：也召唤虫子，按价值决定数量；免费礼物不触发
	default:
		if rules.UnlistedGiftAction == "none" {
			log.Printf("[B站礼物] %s(UID:%d, %s) 送出「%s」x%d (未配置触发规则，unlisted_gift_action=none 不处理)", g.Uname, g.Uid, role, g.GiftName, num)
			return nil
		}
		if isFreeGift(g) {
			log.Printf("[B站礼物] %s(UID:%d, %s) 送出免费礼物「%s」x%d (未配置触发规则，免费礼物不触发)", g.Uname, g.Uid, role, g.GiftName, num)
			return nil
		}
		return randomGiftOutcome(rules, g, num, bugsForUnlistedGift(g, num), role)
	}
}

func startBilibiliClient(cfg *Config, hub *WSHub) (stop func()) {
	if cfg.RoomID <= 0 {
		log.Printf("[B站] config.json 中 room_id 为 0，当前处于本地调试模式（请填写直播间号后重启）")
		return func() {}
	}

	c := client.NewClient(cfg.RoomID)

	// 打印直播间弹幕日志（仅终端显示，不触发游戏交互）
	c.OnDanmaku(func(d *message.Danmaku) {
		if d.Sender != nil {
			log.Printf("[B站弹幕] %s(UID:%d): %s", d.Sender.Uname, d.Sender.Uid, d.Content)
		} else {
			log.Printf("[B站弹幕] %s", d.Content)
		}
	})

	// 打印醒目留言 (SC) 日志
	c.OnSuperChat(func(sc *message.SuperChat) {
		log.Printf("[B站SC] %s (¥%d): %s", sc.UserInfo.Uname, sc.Price, sc.Message)
	})

	// 打印上舰日志
	c.OnGuardBuy(func(gb *message.GuardBuy) {
		log.Printf("[B站上舰] %s(UID:%d) 开通了「%s」(%s) x%d", gb.Username, gb.Uid, gb.GiftName, guardLevelName(gb.GuardLevel), gb.Num)
		switch gb.GuardLevel {
		case 1, 2:
			ev := admiralEvent(&cfg.Rules, gb.Uid, gb.Username, "", gb.GiftName, 3, 0)
			log.Printf("[B站上舰] -> 👑 召唤提督至尊舰船(%s)，当天常驻（北京时间 24 点消失）", ev.ShipClass)
			hub.Broadcast(ev)
		case 3:
			log.Printf("[B站上舰] -> 🛡️ 召唤新舰长守护战舰，自动守护 %d 分钟", cfg.Rules.NewGuardShipSec/60)
			hub.Broadcast(guardShipEvent(&cfg.Rules, gb.Uid, gb.Username, gb.GiftName))
		}
	})

	// 监听礼物事件（唯一交互入口）：解析成游戏事件后推给画布
	onGiftFn := func(g *message.Gift) {
		log.Printf("🎁 [B站礼物-原始] uid=%d uname=%q giftName=%q originalGiftName=%q num=%d price=%d coin=%s tid=%s guard=%d medalGuard=%d",
			g.Uid, g.Uname, g.GiftName, g.OriginalGiftName, g.Num, g.Price, g.CoinType, g.Tid, g.GuardLevel, g.MedalInfo.GuardLevel)
		num := g.Num
		if num <= 0 {
			num = 1
		}
		for _, ev := range classifyGift(&cfg.Rules, g) {
			if ev.Type == "blind_box" {
				ev.Batteries = giftBatteries(g, 1) // 盲盒每开一个算一个的价钱
			} else {
				ev.Batteries = giftBatteries(g, num)
			}
			hub.Broadcast(ev)
		}
	}
	c.OnGift(onGiftFn)

	stopCh := make(chan struct{})
	go runBilibiliWSLoop(c, cfg.RoomID, cfg.Cookie, onGiftFn, stopCh)
	return func() {
		select {
		case <-stopCh:
		default:
			close(stopCh)
		}
	}
}

// serveStaticOrEmbedded 优先读程序同目录下的文件（方便主播自定义替换），不存在时回退到内置嵌入文件
func serveStaticOrEmbedded(exeDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		if rel == "" {
			rel = "index.html"
		}
		clean := filepath.Clean(rel)
		if strings.HasPrefix(clean, "..") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// 1. 优先尝试从本地目录读取
		localPath := filepath.Join(exeDir, clean)
		if info, err := os.Stat(localPath); err == nil && !info.IsDir() {
			http.ServeFile(w, r, localPath)
			return
		}

		// 2. 回退到内嵌资源（单文件 exe 开箱即用）
		data, err := embeddedFS.ReadFile(filepath.ToSlash(clean))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(clean, ".html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else if strings.HasSuffix(clean, ".jpg") || strings.HasSuffix(clean, ".jpeg") {
			w.Header().Set("Content-Type", "image/jpeg")
		} else if strings.HasSuffix(clean, ".png") {
			w.Header().Set("Content-Type", "image/png")
		}
		_, _ = w.Write(data)
	})
}

func main() {
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	// go run 临时目录时回退到当前工作目录
	if strings.Contains(exeDir, "go-build") {
		if wd, err := os.Getwd(); err == nil {
			exeDir = wd
		}
	}

	defaultCfg := filepath.Join(exeDir, "config.json")
	cfgPath := flag.String("config", defaultCfg, "配置文件路径")
	roomOverride := flag.Int("room", 0, "临时指定 B站直播间号（覆盖 config.json）")
	debugFlag := flag.Bool("debug", false, "打开详细调试日志（打印每条 B站消息）")
	flag.Parse()

	cfg, err := loadOrCreateConfig(*cfgPath)
	if err != nil {
		log.Fatalf("读取配置文件 %s 失败: %v", *cfgPath, err)
	}
	debugMode = cfg.Debug || *debugFlag
	log.Printf("[配置] 读取 %s：房间=%d 登录cookie=%v debug=%v", *cfgPath, cfg.RoomID, cfg.Cookie != "", debugMode)
	if *roomOverride > 0 {
		cfg.RoomID = *roomOverride
	}

	admirals.load(filepath.Join(filepath.Dir(*cfgPath), "admirals_today.json"))

	hub := NewWSHub()
	stopBili := startBilibiliClient(cfg, hub)
	defer stopBili()

	mux := http.NewServeMux()

	// WebSocket 端点
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hub.Add(conn)
		defer hub.Remove(conn)
		// 画布刷新/重连：把本场已召唤的提督舰船补发回去
		for _, ev := range admirals.list() {
			hub.SendTo(conn, ev)
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
		}
	})

	// B站观众头像本地代理
	mux.HandleFunc("/avatar", handleAvatarProxy)

	// 本地模拟送礼接口
	mux.HandleFunc("/api/mock", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		action := q.Get("action")
		user := q.Get("user")
		if user == "" {
			user = "测试观众"
		}
		gift := q.Get("gift")
		count, _ := strconv.Atoi(q.Get("count"))
		if count <= 0 {
			count = 1
		}

		var ev GameEvent
		switch action {
		case "bug":
			if gift == "" {
				gift = "小花花"
			}
			ev = GameEvent{Type: "summon_bug", Count: count, User: user, Gift: gift}
		case "kill":
			if gift == "" {
				gift = "粉丝团灯牌"
			}
			ev = GameEvent{Type: "kill_single", Count: count, User: user, Gift: gift}
		case "captain_lamp", "ship":
			if gift == "" {
				gift = "粉丝团灯牌"
			}
			ev = GameEvent{
				Type:        "ship_lamp",
				Count:       count,
				User:        user,
				Gift:        gift,
				Avatar:      proxyAvatarURL(q.Get("avatar")),
				ShipClass:   q.Get("class"),
				DurationSec: cfg.Rules.ShipDurationSec,
				UltHits:     cfg.Rules.ShipUltHits,
			}
		case "admiral":
			if gift == "" {
				gift = "粉丝团灯牌"
			}
			cls := q.Get("class")
			if cls != "admiral_titan" && cls != "admiral_dragon" {
				cls = pickAdmiralShipClass()
			}
			// count = 送了几个灯牌，每个加 admiral_lamp_auto_sec 秒自动杀虫；count=0 只召唤不加时间
			ev = admirals.register(GameEvent{
				Type: "ship_lamp", Mode: "admiral", Count: count, User: user, Gift: gift,
				Avatar: proxyAvatarURL(q.Get("avatar")), ShipClass: cls,
				UltHits: cfg.Rules.ShipUltHits,
			}, count, cfg.Rules.AdmiralLampAutoSec)
		case "guard":
			if gift == "" {
				gift = "舰长"
			}
			ev = guardShipEvent(&cfg.Rules, 0, user, gift)
		case "gift":
			// 模拟一个普通礼物：price=单价金瓜子(1000=1元，0=免费)
			if gift == "" {
				gift = "测试礼物"
			}
			price, _ := strconv.Atoi(q.Get("price"))
			coin := "gold"
			if price <= 0 {
				coin = "silver"
			}
			g := &message.Gift{Uname: user, GiftName: gift, Num: count, Price: price, CoinType: coin}
			evs := randomGiftOutcome(&cfg.Rules, g, count, bugsForUnlistedGift(g, count), "测试")
			ev = evs[0]
			ev.Batteries = giftBatteries(g, count)
		case "pesticide":
			power, _ := strconv.Atoi(q.Get("power"))
			if power <= 0 {
				power = 50
			}
			ev = GameEvent{Type: "pesticide", Power: power, User: user, Gift: gift}
		case "box":
			if gift == "" {
				gift = "心动盲盒"
			}
			card := q.Get("card")
			if card == "" {
				card = pickBlindBoxCard(cfg.Rules.BlindBoxCards)
			}
			ev = GameEvent{
				Type: "blind_box",
				User: user,
				Gift: gift,
				Card: card,
			}
		default:
			http.Error(w, "usage: /api/mock?action=bug|kill|captain_lamp|guard|admiral|gift|pesticide|box&user=名字&count=1", http.StatusBadRequest)
			return
		}

		// 测试用：手动指定这次礼物值多少电池（测战舰互殴）
		if b, err := strconv.Atoi(q.Get("batteries")); err == nil && b > 0 {
			ev.Batteries = b
		}
		hub.Broadcast(ev)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": ev})
	})

	mux.Handle("/", serveStaticOrEmbedded(exeDir))

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
	}

	go func() {
		log.Printf("========================================================")
		log.Printf("🚀 涂鸦虫虫大作战 · B站直播互动服务端已启动！")
		log.Printf("📺 OBS 全透明无按钮画布: http://127.0.0.1%s/", cfg.ListenAddr)
		log.Printf("🔧 本地调试预览页:       http://127.0.0.1%s/?debug=1", cfg.ListenAddr)
		log.Printf("========================================================")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务启动失败: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("正在优雅关闭服务...")
	_ = srv.Close()
}
