package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"isucon8/isucoin/model"

	"github.com/gorilla/sessions"
	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"
)

const (
	SessionName = "isucoin_session"
)

// ISUCON用初期データの基準時間です
// この時間以降のデータはInitializeで削除されます
var BaseTime = time.Date(2018, 10, 16, 10, 0, 0, 0, time.Local)

var (
	concurrencyLimit = 50
)

type Handler struct {
	db    *sql.DB
	store sessions.Store
}

func init() {
	limit, err := strconv.Atoi(os.Getenv("ISU_CONCURRENCY_LIMIT"))
	if err == nil {
		concurrencyLimit = limit
	}
	log.Println("concurrency limit:", concurrencyLimit)
}

func NewHandler(db *sql.DB, store sessions.Store, _handleTrade bool) *Handler {
	h := &Handler{
		db:    db,
		store: store,
	}
	if _handleTrade {
		model.HandleTrade(h.db)
	}
	h.HandleInfo()
	return h
}

func (h *Handler) Initialize(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	err := h.txScope(func(tx *sql.Tx) error {
		if err := model.InitBenchmark(tx); err != nil {
			return err
		}
		for _, k := range []string{
			model.BankEndpoint,
			model.BankAppid,
			model.LogEndpoint,
			model.LogAppid,
		} {
			if err := model.SetSetting(tx, k, r.FormValue(k)); err != nil {
				return errors.Wrapf(err, "set setting failed. %s", k)
			}
		}
		return nil
	})
	if err != nil {
		h.handleError(w, err, 500)
	} else {
		h.handleSuccess(w, struct{}{})
	}
}

func (h *Handler) Signup(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	name := r.FormValue("name")
	bankID := r.FormValue("bank_id")
	password := r.FormValue("password")
	if name == "" || bankID == "" || password == "" {
		h.handleError(w, errors.New("all parameters are required"), 400)
		return
	}
	err := h.txScope(func(tx *sql.Tx) error {
		return model.UserSignup(tx, name, bankID, password)
	})
	switch {
	case err == model.ErrBankUserNotFound:
		// TODO: 失敗が多いときに403を返すBanの仕様に対応
		h.handleError(w, err, 404)
	case err == model.ErrBankUserConflict:
		h.handleError(w, err, 409)
	case err != nil:
		h.handleError(w, err, 500)
	default:
		h.handleSuccess(w, struct{}{})
	}
}

func (h *Handler) Signin(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	bankID := r.FormValue("bank_id")
	password := r.FormValue("password")
	if bankID == "" || password == "" {
		h.handleError(w, errors.New("all parameters are required"), 400)
		return
	}
	var user *model.User
	var err error
	errInternal := h.txScope(func(tx *sql.Tx) error {
		var errInternal error
		user, err = model.UserLogin(tx, bankID, password)
		if err != model.ErrTooManyFailures && err != model.ErrUserNotFound {
			errInternal = err
		}
		return errInternal
	})
	switch {
	case err == model.ErrTooManyFailures:
		h.handleError(w, err, 403)
	case err == model.ErrUserNotFound:
		h.handleError(w, err, 404)
	case err != nil:
		h.handleError(w, errInternal, 500)
	default:
		session, err := h.store.Get(r, SessionName)
		if err != nil {
			h.handleError(w, err, 500)
			return
		}
		session.Values["user_id"] = user.ID
		if err = session.Save(r, w); err != nil {
			h.handleError(w, err, 500)
			return
		}
		h.handleSuccess(w, user)
	}
}

func (h *Handler) Signout(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	session, err := h.store.Get(r, SessionName)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	session.Values["user_id"] = 0
	session.Options = &sessions.Options{MaxAge: -1}
	if err = session.Save(r, w); err != nil {
		h.handleError(w, err, 500)
		return
	}
	h.handleSuccess(w, struct{}{})
}

var handleInfoOnce sync.Once
var group singleflight.Group
var c = sync.NewCond(new(sync.Mutex))

func (h *Handler) HandleInfo() {
	handleInfoOnce.Do(func(){
		go h.handleInfo()
	})
}

func (h *Handler) handleInfo() {
	ticker := time.Tick(800 * time.Millisecond)
	for {
		select {
		case <- ticker:
			group = singleflight.Group{}
			c.Broadcast()
		}
	}
}

type Res struct {
	cursor int64
	chart_by_sec, chart_by_min, chart_by_hour []model.CandlestickData
	lowest_sell_price, highest_buy_price int64
	enable_share bool
	lastTradeID int64
}

func (h *Handler) info(_cursor string) (Res, error) {
	var (
		err         error
		lastTradeID int64
		lt          = time.Unix(0, 0)
		res         Res
	)
	if _cursor != "" {
		if lastTradeID, _ = strconv.ParseInt(_cursor, 10, 64); lastTradeID > 0 {
			trade, err := model.GetTradeByID(h.db, lastTradeID)
			if err != nil && err != sql.ErrNoRows {
				return Res{}, errors.Wrap(err, "getTradeByID failed")
			}
			if trade != nil {
				lt = trade.CreatedAt
			}
			res.lastTradeID = lastTradeID
		}
	}
	latestTradeID, err := model.GetLatestTradeIDForInfo(h.db)
	if err != nil {
		return Res{}, errors.Wrap(err, "GetLatestTrade failed")
	}
	res.cursor = latestTradeID

	bySecTime := BaseTime.Add(-300 * time.Second)
	if lt.After(bySecTime) {
		bySecTime = time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), lt.Second(), 0, lt.Location())
	}
	res.chart_by_sec, err = model.GetCandlestickDataBySec(h.db, bySecTime)
	if err != nil {
		return Res{}, errors.Wrap(err, "model.GetCandlestickData by sec")
	}

	byMinTime := BaseTime.Add(-300 * time.Minute)
	if lt.After(byMinTime) {
		byMinTime = time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), 0, 0, lt.Location())
	}
	res.chart_by_min, err = model.GetCandlestickDataByMin(h.db, byMinTime)
	if err != nil {
		return Res{}, errors.Wrap(err, "model.GetCandlestickData by min")
	}

	byHourTime := BaseTime.Add(-48 * time.Hour)
	if lt.After(byHourTime) {
		byHourTime = time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), 0, 0, 0, lt.Location())
	}
	res.chart_by_hour, err = model.GetCandlestickDataByHour(h.db, byHourTime)
	if err != nil {
		return Res{}, errors.Wrap(err, "model.GetCandlestickData by hour")
	}

	lowestSellOrder, err := model.GetLowestSellOrder(h.db)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return Res{}, errors.Wrap(err, "model.GetLowestSellOrder")
	default:
		res.lowest_sell_price = lowestSellOrder.Price
	}

	highestBuyOrder, err := model.GetHighestBuyOrder(h.db)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return Res{}, errors.Wrap(err, "model.GetHighestBuyOrder")
	default:
		res.highest_buy_price = highestBuyOrder.Price
	}
	numGoroutine := runtime.NumGoroutine()
	log.Println("NumGoroutine:", numGoroutine)
	enableShare := numGoroutine < concurrencyLimit
	res.enable_share = enableShare

	return res, nil
}

func (h *Handler) Info(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	c.L.Lock()
	c.Wait()
	c.L.Unlock()
	_cursor := r.URL.Query().Get("cursor")
	v, err, _ := group.Do(_cursor, func() (interface{}, error) {
		return h.info(_cursor)
	})
	if err != nil {
		h.handleError(w, err, 500)
	}
	_res := v.(Res)
	res := make(map[string]interface{})
	res["cursor"] = _res.cursor
	res["chart_by_sec"] = _res.chart_by_sec
	res["chart_by_min"] = _res.chart_by_min
	res["chart_by_hour"] = _res.chart_by_hour
	res["lowest_sell_price"] = _res.lowest_sell_price
	res["highest_buy_price"] = _res.highest_buy_price
	res["enable_share"] = _res.enable_share
	lastTradeID := _res.lastTradeID
	user, _ := h.userByRequest(r)
	if user != nil {
		orders, err := model.GetOrdersByUserIDAndLastTradeId(h.db, user.ID, lastTradeID)
		if err != nil {
			h.handleError(w, err, 500)
		}
		for _, order := range orders {
			if err = model.FetchOrderRelation(h.db, order); err != nil {
				h.handleError(w, err, 500)
			}
		}
		res["traded_orders"] = orders
	}
	h.handleSuccess(w, res)
}

func (h *Handler) AddOrders(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	user, err := h.userByRequest(r)
	if err != nil {
		h.handleError(w, err, 401)
		return
	}
	amount, _ := strconv.ParseInt(r.FormValue("amount"), 10, 64)
	price, _ := strconv.ParseInt(r.FormValue("price"), 10, 64)
	var order *model.Order
	err = h.txScope(func(tx *sql.Tx) (err error) {
		order, err = model.AddOrder(tx, r.FormValue("type"), user.ID, amount, price)
		return
	})
	switch {
	case err == model.ErrParameterInvalid || err == model.ErrCreditInsufficient:
		h.handleError(w, err, 400)
	case err != nil:
		h.handleError(w, err, 500)
	default:
		h.handleSuccess(w, map[string]interface{}{
			"id": order.ID,
		})
	}
}

func (h *Handler) GetOrders(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	user, err := h.userByRequest(r)
	if err != nil {
		h.handleError(w, err, 401)
		return
	}
	orders, err := model.GetOrdersByUserID(h.db, user.ID)
	if err != nil {
		h.handleError(w, err, 500)
		return
	}
	for _, order := range orders {
		if err = model.FetchOrderRelation(h.db, order); err != nil {
			h.handleError(w, err, 500)
			return
		}
	}
	h.handleSuccess(w, orders)
}

func (h *Handler) DeleteOrders(w http.ResponseWriter, r *http.Request, p httprouter.Params) {
	user, err := h.userByRequest(r)
	if err != nil {
		h.handleError(w, err, 401)
		return
	}
	id, _ := strconv.ParseInt(p.ByName("id"), 10, 64)
	err = h.txScope(func(tx *sql.Tx) error {
		return model.DeleteOrder(tx, user.ID, id, "canceled")
	})
	switch {
	case err == model.ErrOrderNotFound || err == model.ErrOrderAlreadyClosed:
		h.handleError(w, err, 404)
	case err != nil:
		h.handleError(w, err, 500)
	default:
		h.handleSuccess(w, map[string]interface{}{
			"id": id,
		})
	}
}

func (h *Handler) CommonMiddleware(f http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				h.handleError(w, err, 400)
				return
			}
		}
		session, err := h.store.Get(r, SessionName)
		if err != nil {
			h.handleError(w, err, 500)
			return
		}
		if _userID, ok := session.Values["user_id"]; ok {
			userID := _userID.(int64)
			user, err := model.GetUserByID(h.db, userID)
			switch {
			case err == sql.ErrNoRows:
				session.Values["user_id"] = 0
				session.Options = &sessions.Options{MaxAge: -1}
				if err = session.Save(r, w); err != nil {
					h.handleError(w, err, 500)
					return
				}
				h.handleError(w, errors.New("セッションが切断されました"), 404)
				return
			case err != nil:
				h.handleError(w, err, 500)
				return
			}
			ctx := context.WithValue(r.Context(), "user_id", user.ID)
			f.ServeHTTP(w, r.WithContext(ctx))
		} else {
			f.ServeHTTP(w, r)
		}
	})
}

func (h *Handler) userByRequest(r *http.Request) (*model.User, error) {
	v := r.Context().Value("user_id")
	if id, ok := v.(int64); ok {
		return model.GetUserByID(h.db, id)
	}
	return nil, errors.New("Not authenticated")
}

func (h *Handler) handleSuccess(w http.ResponseWriter, data interface{}) {
	w.WriteHeader(200)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("[WARN] write response json failed. %s", err)
	}
}

func (h *Handler) handleError(w http.ResponseWriter, err error, code int) {
	w.WriteHeader(code)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	log.Printf("[WARN] err: %s", err.Error())
	data := map[string]interface{}{
		"code": code,
		"err":  err.Error(),
	}
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("[WARN] write error response json failed. %s", err)
	}
}

func (h *Handler) txScope(f func(*sql.Tx) error) (err error) {
	var tx *sql.Tx
	tx, err = h.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin transaction failed")
	}
	defer func() {
		if e := recover(); e != nil {
			tx.Rollback()
			err = errors.Errorf("panic in transaction: %s", e)
		} else if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()
	err = f(tx)
	return
}
