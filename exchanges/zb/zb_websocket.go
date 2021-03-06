package zb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thrasher-corp/gocryptotrader/common/crypto"
	"github.com/thrasher-corp/gocryptotrader/currency"
	exchange "github.com/thrasher-corp/gocryptotrader/exchanges"
	"github.com/thrasher-corp/gocryptotrader/exchanges/asset"
	"github.com/thrasher-corp/gocryptotrader/exchanges/orderbook"
	"github.com/thrasher-corp/gocryptotrader/exchanges/ticker"
	"github.com/thrasher-corp/gocryptotrader/exchanges/websocket/wshandler"
	"github.com/thrasher-corp/gocryptotrader/log"
)

const (
	zbWebsocketAPI       = "wss://api.zb.cn:9999/websocket"
	zWebsocketAddChannel = "addChannel"
	zbWebsocketRateLimit = 20
)

// WsConnect initiates a websocket connection
func (z *ZB) WsConnect() error {
	if !z.Websocket.IsEnabled() || !z.IsEnabled() {
		return errors.New(wshandler.WebsocketNotEnabled)
	}
	var dialer websocket.Dialer
	err := z.WebsocketConn.Dial(&dialer, http.Header{})
	if err != nil {
		return err
	}

	go z.WsHandleData()
	z.GenerateDefaultSubscriptions()

	return nil
}

// WsHandleData handles all the websocket data coming from the websocket
// connection
func (z *ZB) WsHandleData() {
	z.Websocket.Wg.Add(1)

	defer func() {
		z.Websocket.Wg.Done()
	}()

	for {
		select {
		case <-z.Websocket.ShutdownC:
			return
		default:
			resp, err := z.WebsocketConn.ReadMessage()
			if err != nil {
				z.Websocket.ReadMessageErrors <- err
				return
			}
			z.Websocket.TrafficAlert <- struct{}{}
			fixedJSON := z.wsFixInvalidJSON(resp.Raw)
			var result Generic
			err = json.Unmarshal(fixedJSON, &result)
			if err != nil {
				z.Websocket.DataHandler <- err
				continue
			}
			if result.No > 0 {
				z.WebsocketConn.AddResponseWithID(result.No, fixedJSON)
				continue
			}
			if result.Code > 0 && result.Code != 1000 {
				z.Websocket.DataHandler <- fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, result.Message, wsErrCodes[result.Code])
				continue
			}
			switch {
			case strings.Contains(result.Channel, "markets"):
				var markets Markets
				err := json.Unmarshal(result.Data, &markets)
				if err != nil {
					z.Websocket.DataHandler <- err
					continue
				}

			case strings.Contains(result.Channel, "ticker"):
				cPair := strings.Split(result.Channel, "_")
				var wsTicker WsTicker
				err := json.Unmarshal(fixedJSON, &wsTicker)
				if err != nil {
					z.Websocket.DataHandler <- err
					continue
				}

				z.Websocket.DataHandler <- &ticker.Price{
					ExchangeName: z.Name,
					Close:        wsTicker.Data.Last,
					Volume:       wsTicker.Data.Volume24Hr,
					High:         wsTicker.Data.High,
					Low:          wsTicker.Data.Low,
					Last:         wsTicker.Data.Last,
					Bid:          wsTicker.Data.Buy,
					Ask:          wsTicker.Data.Sell,
					LastUpdated:  time.Unix(0, wsTicker.Date*int64(time.Millisecond)),
					AssetType:    asset.Spot,
					Pair:         currency.NewPairFromString(cPair[0]),
				}

			case strings.Contains(result.Channel, "depth"):
				var depth WsDepth
				err := json.Unmarshal(fixedJSON, &depth)
				if err != nil {
					z.Websocket.DataHandler <- err
					continue
				}

				var asks []orderbook.Item
				for i := range depth.Asks {
					asks = append(asks, orderbook.Item{
						Amount: depth.Asks[i][1].(float64),
						Price:  depth.Asks[i][0].(float64),
					})
				}

				var bids []orderbook.Item
				for i := range depth.Bids {
					bids = append(bids, orderbook.Item{
						Amount: depth.Bids[i][1].(float64),
						Price:  depth.Bids[i][0].(float64),
					})
				}

				channelInfo := strings.Split(result.Channel, "_")
				cPair := currency.NewPairFromString(channelInfo[0])
				var newOrderBook orderbook.Base
				newOrderBook.Asks = asks
				newOrderBook.Bids = bids
				newOrderBook.AssetType = asset.Spot
				newOrderBook.Pair = cPair
				newOrderBook.ExchangeName = z.Name

				err = z.Websocket.Orderbook.LoadSnapshot(&newOrderBook)
				if err != nil {
					z.Websocket.DataHandler <- err
					continue
				}

				z.Websocket.DataHandler <- wshandler.WebsocketOrderbookUpdate{
					Pair:     cPair,
					Asset:    asset.Spot,
					Exchange: z.Name,
				}

			case strings.Contains(result.Channel, "trades"):
				var trades WsTrades
				err := json.Unmarshal(fixedJSON, &trades)
				if err != nil {
					z.Websocket.DataHandler <- err
					continue
				}
				// Most up to date trade
				if len(trades.Data) == 0 {
					continue
				}
				t := trades.Data[len(trades.Data)-1]

				channelInfo := strings.Split(result.Channel, "_")
				cPair := currency.NewPairFromString(channelInfo[0])
				z.Websocket.DataHandler <- wshandler.TradeData{
					Timestamp:    time.Unix(t.Date, 0),
					CurrencyPair: cPair,
					AssetType:    asset.Spot,
					Exchange:     z.Name,
					Price:        t.Price,
					Amount:       t.Amount,
					Side:         t.TradeType,
				}
			default:
				z.Websocket.DataHandler <- errors.New("zb_websocket.go error - unhandled websocket response")
				continue
			}
		}
	}
}

// GenerateDefaultSubscriptions Adds default subscriptions to websocket to be handled by ManageSubscriptions()
func (z *ZB) GenerateDefaultSubscriptions() {
	var subscriptions []wshandler.WebsocketChannelSubscription
	// Tickerdata is its own channel
	subscriptions = append(subscriptions, wshandler.WebsocketChannelSubscription{
		Channel: "markets",
	})
	channels := []string{"%s_ticker", "%s_depth", "%s_trades"}
	enabledCurrencies := z.GetEnabledPairs(asset.Spot)
	for i := range channels {
		for j := range enabledCurrencies {
			enabledCurrencies[j].Delimiter = ""
			subscriptions = append(subscriptions, wshandler.WebsocketChannelSubscription{
				Channel:  fmt.Sprintf(channels[i], enabledCurrencies[j].Lower().String()),
				Currency: enabledCurrencies[j].Lower(),
			})
		}
	}
	z.Websocket.SubscribeToChannels(subscriptions)
}

// Subscribe sends a websocket message to receive data from the channel
func (z *ZB) Subscribe(channelToSubscribe wshandler.WebsocketChannelSubscription) error {
	subscriptionRequest := Subscription{
		Event:   zWebsocketAddChannel,
		Channel: channelToSubscribe.Channel,
	}
	return z.WebsocketConn.SendJSONMessage(subscriptionRequest)
}

func (z *ZB) wsGenerateSignature(request interface{}) string {
	jsonResponse, err := json.Marshal(request)
	if err != nil {
		log.Error(log.ExchangeSys, err)
		return ""
	}
	hmac := crypto.GetHMAC(crypto.HashMD5,
		jsonResponse,
		[]byte(crypto.Sha1ToHex(z.API.Credentials.Secret)))
	return fmt.Sprintf("%x", hmac)
}

func (z *ZB) wsFixInvalidJSON(json []byte) []byte {
	invalidZbJSONRegex := `(\"\[|\"\{)(.*)(\]\"|\}\")`
	regexChecker := regexp.MustCompile(invalidZbJSONRegex)
	matchingResults := regexChecker.Find(json)
	if matchingResults == nil {
		return json
	}
	// Remove first quote character
	capturedInvalidZBJSON := strings.Replace(string(matchingResults), "\"", "", 1)
	// Remove last quote character
	fixedJSON := capturedInvalidZBJSON[:len(capturedInvalidZBJSON)-1]
	return []byte(strings.Replace(string(json), string(matchingResults), fixedJSON, 1))
}

func (z *ZB) wsAddSubUser(username, password string) (*WsGetSubUserListResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsAddSubUserRequest{
		Memo:        "memo",
		Password:    password,
		SubUserName: username,
	}
	request.Channel = "addSubUser"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.No = z.WebsocketConn.GenerateMessageID(true)
	request.Sign = z.wsGenerateSignature(request)
	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var genericResponse Generic
	err = json.Unmarshal(resp, &genericResponse)
	if err != nil {
		return nil, err
	}
	if genericResponse.Code > 0 && genericResponse.Code != 1000 {
		return nil, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, genericResponse.Message, wsErrCodes[genericResponse.Code])
	}
	var response WsGetSubUserListResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

func (z *ZB) wsGetSubUserList() (*WsGetSubUserListResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsAuthenticatedRequest{}
	request.Channel = "getSubUserList"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.No = z.WebsocketConn.GenerateMessageID(true)
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsGetSubUserListResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsDoTransferFunds(pair currency.Code, amount float64, fromUserName, toUserName string) (*WsRequestResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsDoTransferFundsRequest{
		Amount:       amount,
		Currency:     pair,
		FromUserName: fromUserName,
		ToUserName:   toUserName,
		No:           z.WebsocketConn.GenerateMessageID(true),
	}
	request.Channel = "doTransferFunds"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsRequestResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsCreateSubUserKey(assetPerm, entrustPerm, leverPerm, moneyPerm bool, keyName, toUserID string) (*WsRequestResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsCreateSubUserKeyRequest{
		AssetPerm:   assetPerm,
		EntrustPerm: entrustPerm,
		KeyName:     keyName,
		LeverPerm:   leverPerm,
		MoneyPerm:   moneyPerm,
		No:          z.WebsocketConn.GenerateMessageID(true),
		ToUserID:    toUserID,
	}
	request.Channel = "createSubUserKey"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsRequestResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsSubmitOrder(pair currency.Pair, amount, price float64, tradeType int64) (*WsSubmitOrderResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsSubmitOrderRequest{
		Amount:    amount,
		Price:     price,
		TradeType: tradeType,
		No:        z.WebsocketConn.GenerateMessageID(true),
	}
	request.Channel = pair.String() + "_order"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsSubmitOrderResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsCancelOrder(pair currency.Pair, orderID int64) (*WsCancelOrderResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsCancelOrderRequest{
		ID: orderID,
		No: z.WebsocketConn.GenerateMessageID(true),
	}
	request.Channel = pair.String() + "_cancelorder"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsCancelOrderResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsGetOrder(pair currency.Pair, orderID int64) (*WsGetOrderResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsGetOrderRequest{
		ID: orderID,
		No: z.WebsocketConn.GenerateMessageID(true),
	}
	request.Channel = pair.String() + "_getorder"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsGetOrderResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsGetOrders(pair currency.Pair, pageIndex, tradeType int64) (*WsGetOrdersResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsGetOrdersRequest{
		PageIndex: pageIndex,
		TradeType: tradeType,
		No:        z.WebsocketConn.GenerateMessageID(true),
	}
	request.Channel = pair.String() + "_getorders"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.Sign = z.wsGenerateSignature(request)
	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsGetOrdersResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsGetOrdersIgnoreTradeType(pair currency.Pair, pageIndex, pageSize int64) (*WsGetOrdersIgnoreTradeTypeResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsGetOrdersIgnoreTradeTypeRequest{
		PageIndex: pageIndex,
		PageSize:  pageSize,
		No:        z.WebsocketConn.GenerateMessageID(true),
	}
	request.Channel = pair.String() + "_getordersignoretradetype"
	request.Event = zWebsocketAddChannel
	request.Accesskey = z.API.Credentials.Key
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsGetOrdersIgnoreTradeTypeResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}

func (z *ZB) wsGetAccountInfoRequest() (*WsGetAccountInfoResponse, error) {
	if !z.GetAuthenticatedAPISupport(exchange.WebsocketAuthentication) {
		return nil, fmt.Errorf("%v AuthenticatedWebsocketAPISupport not enabled", z.Name)
	}
	request := WsAuthenticatedRequest{
		Channel:   "getaccountinfo",
		Event:     zWebsocketAddChannel,
		Accesskey: z.API.Credentials.Key,
		No:        z.WebsocketConn.GenerateMessageID(true),
	}
	request.Sign = z.wsGenerateSignature(request)

	resp, err := z.WebsocketConn.SendMessageReturnResponse(request.No, request)
	if err != nil {
		return nil, err
	}
	var response WsGetAccountInfoResponse
	err = json.Unmarshal(resp, &response)
	if err != nil {
		return nil, err
	}
	if response.Code > 0 && response.Code != 1000 {
		return &response, fmt.Errorf("%v request failed, message: %v, error code: %v", z.Name, response.Message, wsErrCodes[response.Code])
	}
	return &response, nil
}
