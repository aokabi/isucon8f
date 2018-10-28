package model

import (
	"database/sql"
	"isucon8/isubank"
	"time"
	"github.com/go-sql-driver/mysql"
	"github.com/pkg/errors"
)

const (
	OrderTypeBuy  = "buy"
	OrderTypeSell = "sell"
)

//go:generate scanner
type Order struct {
	ID        int64      `json:"id"`
	Type      string     `json:"type"`
	UserID    int64      `json:"user_id"`
	Amount    int64      `json:"amount"`
	Price     int64      `json:"price"`
	ClosedAt  *time.Time `json:"closed_at"`
	TradeID   int64      `json:"trade_id,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	User      *User      `json:"user,omitempty"`
	Trade     *Trade     `json:"trade,omitempty"`
}

func GetOrdersByUserID(d QueryExecutor, userID int64) ([]*Order, error) {
	return scanOrders(d.Query("SELECT * FROM orders WHERE user_id = ? AND (closed_at IS NULL OR trade_id IS NOT NULL) ORDER BY created_at ASC", userID))
}

func GetOrdersByUserIDAndLastTradeId(d QueryExecutor, userID int64, tradeID int64) ([]*Order, error) {
	rows, err := d.Query(`SELECT o.id, o.type, o.user_id, o.amount, o.price, o.closed_at, o.trade_id, o.created_at, u.name, t.amount, t.price, t.created_at FROM orders AS o INNER JOIN user AS u ON o.user_id = u.id LEFT JOIN trade AS t ON o.trade_id = t.id WHERE o.user_id = ? AND o.trade_id IS NOT NULL AND o.trade_id > ? ORDER BY o.created_at ASC`, userID, tradeID)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = rows.Close()
	}()
	orders := []*Order{}
	for rows.Next() {
		var v Order
		var closedAt mysql.NullTime
		var tradeID sql.NullInt64
		var name string
		var amount, price sql.NullInt64
		var created_at mysql.NullTime
		if err = rows.Scan(&v.ID, &v.Type, &v.UserID, &v.Amount, &v.Price, &closedAt, &tradeID, &v.CreatedAt, &name, &amount, &price, &created_at); err != nil {
			return nil, err
		}
		if closedAt.Valid {
			v.ClosedAt = &closedAt.Time
		}
		if tradeID.Valid {
			v.TradeID = tradeID.Int64
		}
		v.User = &User{ID: v.UserID, Name: name}
		v.Trade = &Trade{}
		if amount.Valid {
			v.Trade.Amount = amount.Int64
		}
		if price.Valid {
			v.Trade.Price = price.Int64
		}
		if created_at.Valid {
			v.Trade.CreatedAt = created_at.Time
		}
		orders = append(orders, &v)
	}
	return orders, rows.Err()
}

func getOpenOrderByID(tx *sql.Tx, id int64) (*Order, error) {
	order, err := getOrderByIDWithLock(tx, id)
	if err != nil {
		return nil, errors.Wrap(err, "getOrderByIDWithLock sell_order")
	}
	if order.ClosedAt != nil {
		return nil, ErrOrderAlreadyClosed
	}
	order.User, err = getUserByIDWithLock(tx, order.UserID)
	if err != nil {
		return nil, errors.Wrap(err, "getUserByIDWithLock sell user")
	}
	return order, nil
}

func GetOrderByID(d QueryExecutor, id int64) (*Order, error) {
	return scanOrder(d.Query("SELECT * FROM orders WHERE id = ?", id))
}

func getOrderByIDWithLock(tx *sql.Tx, id int64) (*Order, error) {
	return scanOrder(tx.Query("SELECT * FROM orders WHERE id = ? FOR UPDATE", id))
}

func GetLowestSellOrder(d QueryExecutor) (*Order, error) {
	return scanOrder(d.Query("SELECT * FROM orders WHERE type = ? AND closed_at IS NULL ORDER BY price ASC, created_at ASC LIMIT 1", OrderTypeSell))
}

func GetHighestBuyOrder(d QueryExecutor) (*Order, error) {
	return scanOrder(d.Query("SELECT * FROM orders WHERE type = ? AND closed_at IS NULL ORDER BY price DESC, created_at ASC LIMIT 1", OrderTypeBuy))
}

func FetchOrderRelation(d *sql.DB, order *Order) error {
	var err error
	order.User, err = GetUserByID(d, order.UserID)
	if err != nil {
		return errors.Wrapf(err, "GetUserByID failed. id")
	}
	if order.TradeID > 0 {
		order.Trade, err = GetTradeByID(d, order.TradeID)
		if err != nil {
			return errors.Wrapf(err, "GetTradeByID failed. id")
		}
	}
	return nil
}

func AddOrder(tx *sql.Tx, ot string, userID, amount, price int64, bankID string) (int64, error) {
	if amount <= 0 || price <= 0 {
		return 0, ErrParameterInvalid
	}
	bank, err := Isubank(tx)
	if err != nil {
		return 0, errors.Wrap(err, "newIsubank failed")
	}
	switch ot {
	case OrderTypeBuy:
		totalPrice := price * amount
		if err = bank.Check(bankID, totalPrice); err != nil {
			sendLog(tx, "buy.error", map[string]interface{}{
				"error":   err.Error(),
				"user_id": userID,
				"amount":  amount,
				"price":   price,
			})
			if err == isubank.ErrCreditInsufficient {
				return 0, ErrCreditInsufficient
			}
			return 0, errors.Wrap(err, "isubank check failed")
		}
	case OrderTypeSell:
		// TODO 椅子の保有チェック
	default:
		return 0, ErrParameterInvalid
	}
	res, err := tx.Exec(`INSERT INTO orders (type, user_id, amount, price, created_at) VALUES (?, ?, ?, ?, NOW(6))`, ot, userID, amount, price)
	if err != nil {
		return 0, errors.Wrap(err, "insert order failed")
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, errors.Wrap(err, "get order_id failed")
	}
	sendLog(tx, ot+".order", map[string]interface{}{
		"order_id": id,
		"user_id":  userID,
		"amount":   amount,
		"price":    price,
	})
	return id, nil
}

func DeleteOrder(tx *sql.Tx, userID, orderID int64, reason string) error {
	order, err := getOrderByIDWithLock(tx, orderID)
	switch {
	case err == sql.ErrNoRows:
		return ErrOrderNotFound
	case err != nil:
		return errors.Wrapf(err, "getOrderByIDWithLock failed. id")
	case order.UserID != userID:
		return ErrOrderNotFound
	case order.ClosedAt != nil:
		return ErrOrderAlreadyClosed
	}
	return cancelOrder(tx, order, reason)
}

func cancelOrder(d QueryExecutor, order *Order, reason string) error {
	if _, err := d.Exec(`UPDATE orders SET closed_at = NOW(6) WHERE id = ?`, order.ID); err != nil {
		return errors.Wrap(err, "update orders for cancel")
	}
	sendLog(d, order.Type+".delete", map[string]interface{}{
		"order_id": order.ID,
		"user_id":  order.UserID,
		"reason":   reason,
	})
	return nil
}
