package coin

import (
	"github.com/tendermint/go-wire/data"
	"github.com/tendermint/tmlibs/log"

	"github.com/tendermint/basecoin"
	"github.com/tendermint/basecoin/errors"
	"github.com/tendermint/basecoin/modules/auth"
	"github.com/tendermint/basecoin/modules/ibc"
	"github.com/tendermint/basecoin/stack"
	"github.com/tendermint/basecoin/state"
)

//NameCoin - name space of the coin module
const NameCoin = "coin"

// Handler includes an accountant
type Handler struct{}

var _ stack.Dispatchable = Handler{}

// NewHandler - new accountant handler for the coin module
func NewHandler() Handler {
	return Handler{}
}

// Name - return name space
func (Handler) Name() string {
	return NameCoin
}

// AssertDispatcher - to fulfill Dispatchable interface
func (Handler) AssertDispatcher() {}

// CheckTx checks if there is enough money in the account
func (h Handler) CheckTx(ctx basecoin.Context, store state.SimpleDB,
	tx basecoin.Tx, _ basecoin.Checker) (res basecoin.Result, err error) {

	err = tx.ValidateBasic()
	if err != nil {
		return res, err
	}

	switch t := tx.Unwrap().(type) {
	case SendTx:
		return res, h.checkSendTx(ctx, store, t)
	case CreditTx:
		return h.creditTx(ctx, store, t)
	}
	return res, errors.ErrUnknownTxType(tx.Unwrap())
}

// DeliverTx moves the money
func (h Handler) DeliverTx(ctx basecoin.Context, store state.SimpleDB,
	tx basecoin.Tx, cb basecoin.Deliver) (res basecoin.Result, err error) {

	err = tx.ValidateBasic()
	if err != nil {
		return res, err
	}

	switch t := tx.Unwrap().(type) {
	case SendTx:
		return h.sendTx(ctx, store, t, cb)
	case CreditTx:
		return h.creditTx(ctx, store, t)
	}
	return res, errors.ErrUnknownTxType(tx.Unwrap())
}

// SetOption - sets the genesis account balance
func (h Handler) SetOption(l log.Logger, store state.SimpleDB,
	module, key, value string, cb basecoin.SetOptioner) (log string, err error) {
	if module != NameCoin {
		return "", errors.ErrUnknownModule(module)
	}
	switch key {
	case "account":
		return setAccount(store, value)
	case "issuer":
		return setIssuer(store, value)
	}
	return "", errors.ErrUnknownKey(key)
}

func (h Handler) sendTx(ctx basecoin.Context, store state.SimpleDB,
	send SendTx, cb basecoin.Deliver) (res basecoin.Result, err error) {

	err = checkTx(ctx, send)
	if err != nil {
		return res, err
	}

	// deduct from all input accounts
	senders := basecoin.Actors{}
	for _, in := range send.Inputs {
		_, err = ChangeCoins(store, in.Address, in.Coins.Negative())
		if err != nil {
			return res, err
		}
		senders = append(senders, in.Address)
	}

	// add to all output accounts
	for _, out := range send.Outputs {
		// TODO: cleaner way, this makes sure we don't consider
		// incoming ibc packets with our chain to be remote packets
		if out.Address.ChainID == ctx.ChainID() {
			out.Address.ChainID = ""
		}

		_, err = ChangeCoins(store, out.Address, out.Coins)
		if err != nil {
			return res, err
		}
		// now send ibc packet if needed...
		if out.Address.ChainID != "" {
			// FIXME: if there are many outputs, we need to adjust inputs
			// so the amounts in and out match.  how?
			inputs := make([]TxInput, len(send.Inputs))
			for i := range send.Inputs {
				inputs[i] = send.Inputs[i]
				inputs[i].Address = inputs[i].Address.WithChain(ctx.ChainID())
			}

			outTx := NewSendTx(inputs, []TxOutput{out})
			packet := ibc.CreatePacketTx{
				DestChain:   out.Address.ChainID,
				Permissions: senders,
				Tx:          outTx,
			}
			ibcCtx := ctx.WithPermissions(ibc.AllowIBC(NameCoin))
			_, err := cb.DeliverTx(ibcCtx, store, packet.Wrap())
			if err != nil {
				return res, err
			}
		}
	}

	// a-ok!
	return res, nil
}

func (h Handler) creditTx(ctx basecoin.Context, store state.SimpleDB,
	credit CreditTx) (res basecoin.Result, err error) {

	// first check permissions!!
	info, err := loadHandlerInfo(store)
	if err != nil {
		return res, err
	}
	if info.Issuer.Empty() || !ctx.HasPermission(info.Issuer) {
		return res, errors.ErrUnauthorized()
	}

	// load up the account
	addr := ChainAddr(credit.Debitor)
	acct, err := GetAccount(store, addr)
	if err != nil {
		return res, err
	}

	// make and check changes
	acct.Coins = acct.Coins.Plus(credit.Credit)
	if !acct.Coins.IsNonnegative() {
		return res, ErrInsufficientFunds()
	}
	acct.Credit = acct.Credit.Plus(credit.Credit)
	if !acct.Credit.IsNonnegative() {
		return res, ErrInsufficientCredit()
	}

	err = storeAccount(store, addr.Bytes(), acct)
	return res, err
}

func checkTx(ctx basecoin.Context, send SendTx) error {
	// check if all inputs have permission
	for _, in := range send.Inputs {
		if !ctx.HasPermission(in.Address) {
			return errors.ErrUnauthorized()
		}
	}
	return nil
}

func (Handler) checkSendTx(ctx basecoin.Context, store state.SimpleDB, send SendTx) error {
	err := checkTx(ctx, send)
	if err != nil {
		return err
	}
	// now make sure there is money
	for _, in := range send.Inputs {
		_, err := CheckCoins(store, in.Address, in.Coins.Negative())
		if err != nil {
			return err
		}
	}
	return nil
}

func setAccount(store state.SimpleDB, value string) (log string, err error) {
	var acc GenesisAccount
	err = data.FromJSON([]byte(value), &acc)
	if err != nil {
		return "", err
	}
	acc.Balance.Sort()
	addr, err := acc.GetAddr()
	if err != nil {
		return "", ErrInvalidAddress()
	}
	// this sets the permission for a public key signature, use that app
	actor := auth.SigPerm(addr)
	err = storeAccount(store, actor.Bytes(), acc.ToAccount())
	if err != nil {
		return "", err
	}
	return "Success", nil
}

// setIssuer sets a permission for some super-powerful account to
// mint money
func setIssuer(store state.SimpleDB, value string) (log string, err error) {
	var issuer basecoin.Actor
	err = data.FromJSON([]byte(value), &issuer)
	if err != nil {
		return "", err
	}
	err = storeIssuer(store, issuer)
	if err != nil {
		return "", err
	}
	return "Success", nil
}