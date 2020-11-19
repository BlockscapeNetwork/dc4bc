package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/cosmos/cosmos-sdk/crypto/keys"
	"github.com/cosmos/cosmos-sdk/types/rest"
	"github.com/cosmos/cosmos-sdk/x/auth"
	authRest "github.com/cosmos/cosmos-sdk/x/auth/client/rest"
	"github.com/cosmos/cosmos-sdk/x/auth/exported"
	"github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/lidofinance/bulletin/app"
	"io/ioutil"
	"net/http"
	"strconv"
)

type TendermintStorage struct {
	nodeEndpoint string
	chainID      string
	name         string
	password     string

	keybase        keys.Keybase
	accountAddress string
	topic          string
}

func NewTendermintStorage(nodeEndpoint, name, chainID string, topic string, password,
	mnemonic string) (Storage, error) {
	var ts TendermintStorage

	ts.nodeEndpoint = nodeEndpoint
	ts.chainID = chainID
	ts.topic = topic
	ts.name = name
	ts.password = password

	ts.keybase = keys.NewInMemory()
	hdPath := keys.CreateHDPath(0, 0).String()
	info, err := ts.keybase.CreateAccount(name, mnemonic, keys.DefaultBIP39Passphrase, password,
		hdPath, keys.Secp256k1)
	if err != nil {
		return nil, err
	}
	ts.accountAddress = info.GetAddress().String()
	return &ts, nil
}

func (ts *TendermintStorage) Close() error {
	ts.keybase.CloseDB()
	return nil
}

type getAccountResponse struct {
	Height string           `json:"heignt"`
	Result exported.Account `json:"result"`
}

func (ts *TendermintStorage) getAccount(addr string) (exported.Account, error) {
	url := fmt.Sprintf("%s/auth/accounts/%s", ts.nodeEndpoint, addr)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to do HTTP GET request: %w", err)
	}
	defer resp.Body.Close()
	var accountResponse getAccountResponse
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if err = app.MakeCodec().UnmarshalJSON(respBody, &accountResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
	}
	return accountResponse.Result, nil
}

type errorResp struct {
	Error string `json:"error"`
}

func rawPostRequest(url string, contentType string, data []byte) ([]byte, error) {
	resp, err := http.Post(url,
		contentType, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errorResp errorResp
		if err = json.Unmarshal(responseBody, &errorResp); err != nil {
			return nil, fmt.Errorf("failed to unmarshal response: %w", err)
		}
		return nil, fmt.Errorf("%s", errorResp.Error)
	}
	return responseBody, nil
}

type BulletinMessage struct {
	ID         string `json:"id"`
	Creator    string `json:"creator"`
	DKGRoundID string `json:"dkg_round_id"`
	Event      string `json:"event"`
	Data       []byte `json:"data"`
	Signature  []byte `json:"signature"`
	Sender     string `json:"sender"`
	Recipient  string `json:"recipient"`
	Topic      string `json:"topic"`
	Offset     string `json:"offset"`
}

type genTxRequest struct {
	BaseReq    rest.BaseReq `json:"base_req"`
	Creator    string       `json:"creator"`
	DKGRoundID string       `json:"dkg_round_id"`
	Event      string       `json:"event"`
	Data       []byte       `json:"data"`
	Signature  []byte       `json:"signature"`
	Sender     string       `json:"sender"`
	Recipient  string       `json:"recipient"`
	Topic      string       `json:"topic"`
}

func (ts *TendermintStorage) genTx(msg Message) (*types.StdTx, error) {
	var req genTxRequest

	req.Creator = ts.accountAddress
	req.DKGRoundID = msg.DkgRoundID
	req.Event = msg.Event
	req.Data = msg.Data
	req.Signature = msg.Signature
	req.Sender = msg.SenderAddr
	req.Recipient = msg.RecipientAddr
	req.Topic = ts.topic

	req.BaseReq.ChainID = ts.chainID
	req.BaseReq.From = ts.accountAddress
	req.Topic = ts.topic

	data, err := app.MakeCodec().MarshalJSON(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	resp, err := rawPostRequest(fmt.Sprintf("%s/bulletin/message", ts.nodeEndpoint),
		"application/json", data)
	if err != nil {
		return nil, err
	}

	var tx auth.StdTx
	if err = app.MakeCodec().UnmarshalJSON(resp, &tx); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &tx, nil
}

func (ts *TendermintStorage) signTx(tx types.StdTx) (*types.StdTx, error) {
	account, err := ts.getAccount(ts.accountAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to get account info: %w", err)
	}
	txBuilder := auth.NewTxBuilder(auth.DefaultTxEncoder(app.MakeCodec()), account.GetAccountNumber(),
		account.GetSequence(), tx.GetGas(), 0, false, ts.chainID, tx.GetMemo(),
		tx.Fee.Amount, tx.Fee.GasPrices()).WithKeybase(ts.keybase)
	signedTx, err := txBuilder.SignStdTx(ts.name, ts.password, tx, false)
	if err != nil {
		return nil, fmt.Errorf("failed to sign tx: %w", err)
	}
	return &signedTx, nil
}

func (ts *TendermintStorage) broadcastTx(tx types.StdTx) error {
	var req authRest.BroadcastReq
	req.Tx = tx
	req.Mode = "block"

	data, err := app.MakeCodec().MarshalJSON(req)
	if err != nil {
		return err
	}
	if _, err = rawPostRequest(fmt.Sprintf("%s/txs", ts.nodeEndpoint),
		"application/json", data); err != nil {
		return err
	}
	return nil
}

func (ts *TendermintStorage) Send(msg Message) (Message, error) {
	tx, err := ts.genTx(msg)
	if err != nil {
		return msg, fmt.Errorf("failed to generate tx: %w", err)
	}
	signedTx, err := ts.signTx(*tx)
	if err != nil {
		return msg, fmt.Errorf("failed to sign tx: %w", err)
	}
	if err = ts.broadcastTx(*signedTx); err != nil {
		return msg, fmt.Errorf("failed to broadcast tx: %w", err)
	}
	return msg, nil
}

type getMessagesResponse struct {
	Height string            `json:"height"`
	Result []BulletinMessage `json:"result"`
}

func (ts *TendermintStorage) GetMessages(offset uint64) ([]Message, error) {
	url := fmt.Sprintf("%s/bulletin/message/%s/%d", ts.nodeEndpoint, ts.topic, offset)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to do HTTP GET request: %w", err)
	}
	defer resp.Body.Close()

	var messagesResponse getMessagesResponse
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var errorResp errorResp
		if err = json.Unmarshal(respBody, &errorResp); err != nil {
			return nil, fmt.Errorf("failed to unmarshal response: %w", err)
		}
		return nil, fmt.Errorf("%w", err)
	}

	if err = json.Unmarshal(respBody, &messagesResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
	}

	msgs := make([]Message, len(messagesResponse.Result))
	for i, message := range messagesResponse.Result {
		parsedOffset, err := strconv.ParseUint(message.Offset, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse offset: %w", err)
		}
		msgs[i] = Message{
			ID:            message.ID,
			DkgRoundID:    message.DKGRoundID,
			Offset:        parsedOffset,
			Event:         message.Topic,
			Data:          message.Data,
			Signature:     message.Signature,
			SenderAddr:    message.Sender,
			RecipientAddr: message.Recipient,
		}
	}
	return msgs, nil
}