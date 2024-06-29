package oracle

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cosmossdk.io/math"
	"github.com/InjectiveLabs/metrics"
	oracletypes "github.com/InjectiveLabs/sdk-go/chain/oracle/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/gorilla/websocket"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
	"github.com/shopspring/decimal"
	log "github.com/xlab/suplog"
)

var _ PricePuller = &storkPriceFeed{}

type StorkFeedConfig struct {
	ProviderName string `toml:"provider"`
	Ticker       string `toml:"ticker"`
	PullInterval string `toml:"pullInterval"`
	Url          string `toml:"url"`
	Header       string `toml:"header"`
	Message      string `toml:"message"`
	OracleType   string `toml:"oracleType"`
}

type StorkConfig struct {
}

type storkPriceFeed struct {
	ticker       string
	providerName string
	interval     time.Duration
	url          string
	header       string
	message      string

	logger  log.Logger
	svcTags metrics.Tags

	oracleType oracletypes.OracleType
}

func ParseStorkFeedConfig(body []byte) (*StorkFeedConfig, error) {
	var config StorkFeedConfig
	if err := toml.Unmarshal(body, &config); err != nil {
		err = errors.Wrap(err, "failed to unmarshal TOML config")
		return nil, err
	}

	return &config, nil
}

// NewStorkPriceFeed returns price puller
func NewStorkPriceFeed(cfg *StorkFeedConfig) (PricePuller, error) {
	pullInterval := 1 * time.Minute
	if len(cfg.PullInterval) > 0 {
		interval, err := time.ParseDuration(cfg.PullInterval)
		if err != nil {
			err = errors.Wrapf(err, "failed to parse pull interval: %s (expected format: 60s)", cfg.PullInterval)
			return nil, err
		}

		if interval < 1*time.Second {
			err = errors.Wrapf(err, "failed to parse pull interval: %s (minimum interval = 1s)", cfg.PullInterval)
			return nil, err
		}

		pullInterval = interval
	}

	var oracleType oracletypes.OracleType
	if cfg.OracleType == "" {
		oracleType = oracletypes.OracleType_PriceFeed
	} else {
		tmpType, exist := oracletypes.OracleType_value[cfg.OracleType]
		if !exist {
			return nil, fmt.Errorf("oracle type does not exist: %s", cfg.OracleType)
		}

		oracleType = oracletypes.OracleType(tmpType)
	}

	feed := &storkPriceFeed{
		ticker:       cfg.Ticker,
		providerName: cfg.ProviderName,
		interval:     pullInterval,
		url:          cfg.Url,
		header:       cfg.Header,
		message:      cfg.Message,
		oracleType:   oracleType,

		logger: log.WithFields(log.Fields{
			"svc":      "oracle",
			"dynamic":  true,
			"provider": cfg.ProviderName,
		}),

		svcTags: metrics.Tags{
			"provider": cfg.ProviderName,
		},
	}

	return feed, nil
}

func (f *storkPriceFeed) Interval() time.Duration {
	return f.interval
}

func (f *storkPriceFeed) Symbol() string {
	return f.ticker
}

func (f *storkPriceFeed) Provider() FeedProvider {
	return FeedProviderStork
}

func (f *storkPriceFeed) ProviderName() string {
	return f.providerName
}

func (f *storkPriceFeed) OracleType() oracletypes.OracleType {
	return oracletypes.OracleType_Stork
}

// PullAssetPair pulls asset pair for an asset id
func (f *storkPriceFeed) PullAssetPair(ctx context.Context) (assetPairs oracletypes.AssetPair, err error) {
	metrics.ReportFuncCall(f.svcTags)
	doneFn := metrics.ReportFuncTiming(f.svcTags)
	defer doneFn()

	// Parse the URL
	u, err := url.Parse(f.url)
	if err != nil {
		log.Fatal("Error parsing URL:", err)
		return oracletypes.AssetPair{}, nil
	}
	header := http.Header{}
	header.Add("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(f.header)))

	dialer := websocket.DefaultDialer
	dialer.EnableCompression = true

	// Connect to the WebSocket server
	conn, resp, err := dialer.Dial(u.String(), header)
	if err != nil {
		if resp != nil {
			log.Printf("Handshake failed with status: %d\n", resp.StatusCode)
			for k, v := range resp.Header {
				log.Printf("%s: %v\n", k, v)
			}
		}
		log.Fatal("Error connecting to WebSocket:", err)
		return oracletypes.AssetPair{}, nil
	}
	defer conn.Close()

	log.Println("Connected to WebSocket server:", resp.Status)

	err = conn.WriteMessage(websocket.TextMessage, []byte(f.message))
	if err != nil {
		log.Fatal("Error writing message:", err)
		return oracletypes.AssetPair{}, nil
	}

	var msgNeed []byte
	count := 0
	for count < 2 {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Println("Error reading message:", err)
			return oracletypes.AssetPair{}, nil

		}
		msgNeed = message
		count += 1
	}

	log.Println("Interrupt received, closing connection")

	var msgResp messageResponse
	if err = json.Unmarshal(msgNeed, &msgResp); err != nil {
		return oracletypes.AssetPair{}, nil
	}
	assetIds := make([]string, 0)
	for key := range msgResp.Data {
		assetIds = append(assetIds, key)
	}
	assetPairs = ConvertDataToAssetPair(msgResp.Data[assetIds[0]], assetIds[0])

	return assetPairs, nil
}

func (f *storkPriceFeed) PullPrice(ctx context.Context) (
	price decimal.Decimal,
	err error,
) {
	return zeroPrice, nil
}

// ConvertDataToAssetPair converts data get from websocket to list of asset pairs
func ConvertDataToAssetPair(data Data, assetId string) (result oracletypes.AssetPair) {
	signedPricesOfAssetPair := []*oracletypes.SignedPriceOfAssetPair{}
	for i := range data.SignedPrices {
		newSignedPriceAssetPair := ConvertSignedPrice(data.SignedPrices[i])
		signedPricesOfAssetPair = append(signedPricesOfAssetPair, &newSignedPriceAssetPair)
	}
	result.SignedPrices = signedPricesOfAssetPair
	result.AssetId = assetId

	return result
}

// ConvertSignedPrice converts signed price to SignedPriceOfAssetPair of Stork
func ConvertSignedPrice(signeds SignedPrice) oracletypes.SignedPriceOfAssetPair {
	var signedPriceOfAssetPair oracletypes.SignedPriceOfAssetPair

	signature := CombineSignatureToString(signeds.TimestampedSignature.Signature)

	signedPriceOfAssetPair.Signature = common.Hex2Bytes(signature)
	signedPriceOfAssetPair.PublisherKey = signeds.PublisherKey
	signedPriceOfAssetPair.Timestamp = signeds.TimestampedSignature.Timestamp
	signedPriceOfAssetPair.Price = signeds.Price

	return signedPriceOfAssetPair
}

// CombineSignatureToString combines a signature to a string
func CombineSignatureToString(signature Signature) (result string) {
	prunedR := strings.TrimPrefix(signature.R, "0x")
	prunedS := strings.TrimPrefix(signature.S, "0x")
	prunedV := strings.TrimPrefix(signature.V, "0x")

	return prunedR + prunedS + prunedV
}

type messageResponse struct {
	Type    string          `json:"type"`
	TraceID string          `json:"trace_id"`
	Data    map[string]Data `json:"data"`
}

type Data struct {
	Timestamp     int64         `json:"timestamp"`
	AssetID       string        `json:"asset_id"`
	SignatureType string        `json:"signature_type"`
	Trigger       string        `json:"trigger"`
	Price         string        `json:"price"`
	SignedPrices  []SignedPrice `json:"signed_prices"`
}

type SignedPrice struct {
	PublisherKey         string               `json:"publisher_key"`
	ExternalAssetID      string               `json:"external_asset_id"`
	SignatureType        string               `json:"signature_type"`
	Price                math.LegacyDec       `json:"price"`
	TimestampedSignature TimestampedSignature `json:"timestamped_signature"`
}

type TimestampedSignature struct {
	Signature Signature `json:"signature"`
	Timestamp uint64    `json:"timestamp"`
	MsgHash   string    `json:"msg_hash"`
}

type Signature struct {
	R string `json:"r"`
	S string `json:"s"`
	V string `json:"v"`
}