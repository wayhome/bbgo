package irr

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/data/tsv"
	"github.com/c9s/bbgo/pkg/datatype/floats"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/indicator"
	"github.com/c9s/bbgo/pkg/pricesolver"
	"github.com/c9s/bbgo/pkg/types"

	"github.com/sirupsen/logrus"
)

const ID = "irr"

var one = fixedpoint.One
var zero = fixedpoint.Zero

var log = logrus.WithField("strategy", ID)

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type Strategy struct {
	Environment *bbgo.Environment
	Symbol      string `json:"symbol"`
	Market      types.Market

	types.IntervalWindow

	// persistence fields
	Position    *types.Position    `persistence:"position"`
	ProfitStats *types.ProfitStats `persistence:"profit_stats"`
	TradeStats  *types.TradeStats  `persistence:"trade_stats"`

	activeOrders *bbgo.ActiveOrderBook

	ExitMethods   bbgo.ExitMethodSet `json:"exits"`
	session       *bbgo.ExchangeSession
	orderExecutor *bbgo.GeneralOrderExecutor

	bbgo.QuantityOrAmount

	// for negative return rate
	nrr *NRR

	stopC chan struct{}

	// StrategyController
	bbgo.StrategyController

	AccountValueCalculator *bbgo.AccountValueCalculator

	// whether to draw graph or not by the end of backtest
	DrawGraph       bool   `json:"drawGraph"`
	GraphPNLPath    string `json:"graphPNLPath"`
	GraphCumPNLPath string `json:"graphCumPNLPath"`

	// for position
	buyPrice     float64 `persistence:"buy_price"`
	sellPrice    float64 `persistence:"sell_price"`
	highestPrice float64 `persistence:"highest_price"`
	lowestPrice  float64 `persistence:"lowest_price"`

	// Accumulated profit report
	AccumulatedProfitReport *AccumulatedProfitReport `json:"accumulatedProfitReport"`

	// 买入价格比当前价格低的比例
	BidSpread fixedpoint.Value `json:"bidSpread"`

	// 卖出价格比当前价格高的比例
	AskSpread fixedpoint.Value `json:"askSpread"`

	// 止盈止损设置
	StopLoss     fixedpoint.Value `json:"stopLoss"`     // 止损比例
	TakeProfit   fixedpoint.Value `json:"takeProfit"`   // 止盈比例
	TrailingStop bool             `json:"trailingStop"` // 是否启用追踪止损

	// 风险管理参数
	MaxDrawdown       fixedpoint.Value `json:"maxDrawdown"`       // 最大回撤限制
	DailyLossLimit    fixedpoint.Value `json:"dailyLossLimit"`    // 每日亏损限制
	PositionSizeLimit fixedpoint.Value `json:"positionSizeLimit"` // 最大仓位限制
}

// AccumulatedProfitReport For accumulated profit report output
type AccumulatedProfitReport struct {
	// AccumulatedProfitMAWindow Accumulated profit SMA window, in number of trades
	AccumulatedProfitMAWindow int `json:"accumulatedProfitMAWindow"`

	// IntervalWindow interval window, in days
	IntervalWindow int `json:"intervalWindow"`

	// NumberOfInterval How many intervals to output to TSV
	NumberOfInterval int `json:"NumberOfInterval"`

	// TsvReportPath The path to output report to
	TsvReportPath string `json:"tsvReportPath"`

	// AccumulatedDailyProfitWindow The window to sum up the daily profit, in days
	AccumulatedDailyProfitWindow int `json:"accumulatedDailyProfitWindow"`

	// Accumulated profit
	accumulatedProfit         fixedpoint.Value
	accumulatedProfitPerDay   floats.Slice
	previousAccumulatedProfit fixedpoint.Value

	// Accumulated profit MA
	accumulatedProfitMA       *indicator.SMA
	accumulatedProfitMAPerDay floats.Slice

	// Daily profit
	dailyProfit floats.Slice

	// Accumulated fee
	accumulatedFee       fixedpoint.Value
	accumulatedFeePerDay floats.Slice

	// Win ratio
	winRatioPerDay floats.Slice

	// Profit factor
	profitFactorPerDay floats.Slice

	// Trade number
	dailyTrades               floats.Slice
	accumulatedTrades         int
	previousAccumulatedTrades int
}

func (r *AccumulatedProfitReport) Initialize() {
	if r.AccumulatedProfitMAWindow <= 0 {
		r.AccumulatedProfitMAWindow = 60
	}
	if r.IntervalWindow <= 0 {
		r.IntervalWindow = 7
	}
	if r.AccumulatedDailyProfitWindow <= 0 {
		r.AccumulatedDailyProfitWindow = 7
	}
	if r.NumberOfInterval <= 0 {
		r.NumberOfInterval = 1
	}
	r.accumulatedProfitMA = &indicator.SMA{IntervalWindow: types.IntervalWindow{Interval: types.Interval1d, Window: r.AccumulatedProfitMAWindow}}
}

func (r *AccumulatedProfitReport) RecordProfit(profit fixedpoint.Value) {
	r.accumulatedProfit = r.accumulatedProfit.Add(profit)
}

func (r *AccumulatedProfitReport) RecordTrade(fee fixedpoint.Value) {
	r.accumulatedFee = r.accumulatedFee.Add(fee)
	r.accumulatedTrades += 1
}

func (r *AccumulatedProfitReport) DailyUpdate(tradeStats *types.TradeStats) {
	// Daily profit
	r.dailyProfit.Update(r.accumulatedProfit.Sub(r.previousAccumulatedProfit).Float64())
	r.previousAccumulatedProfit = r.accumulatedProfit

	// Accumulated profit
	r.accumulatedProfitPerDay.Update(r.accumulatedProfit.Float64())

	// Accumulated profit MA
	r.accumulatedProfitMA.Update(r.accumulatedProfit.Float64())
	r.accumulatedProfitMAPerDay.Update(r.accumulatedProfitMA.Last(0))

	// Accumulated Fee
	r.accumulatedFeePerDay.Update(r.accumulatedFee.Float64())

	// Win ratio
	r.winRatioPerDay.Update(tradeStats.WinningRatio.Float64())

	// Profit factor
	r.profitFactorPerDay.Update(tradeStats.ProfitFactor.Float64())

	// Daily trades
	r.dailyTrades.Update(float64(r.accumulatedTrades - r.previousAccumulatedTrades))
	r.previousAccumulatedTrades = r.accumulatedTrades
}

// Output Accumulated profit report to a TSV file
func (r *AccumulatedProfitReport) Output(symbol string) {
	if r.TsvReportPath != "" {
		tsvwiter, err := tsv.AppendWriterFile(r.TsvReportPath)
		if err != nil {
			panic(err)
		}
		defer tsvwiter.Close()
		// Output symbol, total acc. profit, acc. profit 60MA, interval acc. profit, fee, win rate, profit factor
		_ = tsvwiter.Write([]string{"#", "Symbol", "accumulatedProfit", "accumulatedProfitMA", fmt.Sprintf("%dd profit", r.AccumulatedDailyProfitWindow), "accumulatedFee", "winRatio", "profitFactor", "60D trades"})
		for i := 0; i <= r.NumberOfInterval-1; i++ {
			accumulatedProfit := r.accumulatedProfitPerDay.Index(r.IntervalWindow * i)
			accumulatedProfitStr := fmt.Sprintf("%f", accumulatedProfit)
			accumulatedProfitMA := r.accumulatedProfitMAPerDay.Index(r.IntervalWindow * i)
			accumulatedProfitMAStr := fmt.Sprintf("%f", accumulatedProfitMA)
			intervalAccumulatedProfit := r.dailyProfit.Tail(r.AccumulatedDailyProfitWindow+r.IntervalWindow*i).Sum() - r.dailyProfit.Tail(r.IntervalWindow*i).Sum()
			intervalAccumulatedProfitStr := fmt.Sprintf("%f", intervalAccumulatedProfit)
			accumulatedFee := fmt.Sprintf("%f", r.accumulatedFeePerDay.Index(r.IntervalWindow*i))
			winRatio := fmt.Sprintf("%f", r.winRatioPerDay.Index(r.IntervalWindow*i))
			profitFactor := fmt.Sprintf("%f", r.profitFactorPerDay.Index(r.IntervalWindow*i))
			trades := r.dailyTrades.Tail(60+r.IntervalWindow*i).Sum() - r.dailyTrades.Tail(r.IntervalWindow*i).Sum()
			tradesStr := fmt.Sprintf("%f", trades)

			_ = tsvwiter.Write([]string{fmt.Sprintf("%d", i+1), symbol, accumulatedProfitStr, accumulatedProfitMAStr, intervalAccumulatedProfitStr, accumulatedFee, winRatio, profitFactor, tradesStr})
		}
	}
}

func (s *Strategy) Subscribe(session *bbgo.ExchangeSession) {
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.Interval})
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	return fmt.Sprintf("%s:%s", ID, s.Symbol)
}

func (s *Strategy) Run(ctx context.Context, orderExecutor bbgo.OrderExecutor, session *bbgo.ExchangeSession) error {
	var instanceID = s.InstanceID()

	if s.Position == nil {
		s.Position = types.NewPositionFromMarket(s.Market)
	}

	if s.ProfitStats == nil {
		s.ProfitStats = types.NewProfitStats(s.Market)
	}

	if s.TradeStats == nil {
		s.TradeStats = types.NewTradeStats(s.Symbol)
	}

	// StrategyController
	s.Status = types.StrategyStatusRunning

	s.OnSuspend(func() {
		// Cancel active orders
		_ = s.orderExecutor.GracefulCancel(ctx)
	})

	s.OnEmergencyStop(func() {
		// Cancel active orders
		_ = s.orderExecutor.GracefulCancel(ctx)
		// Close 100% position
		_ = s.orderExecutor.ClosePosition(ctx, fixedpoint.One)
	})

	// initial required information
	s.session = session

	// Set fee rate
	if s.session.MakerFeeRate.Sign() > 0 || s.session.TakerFeeRate.Sign() > 0 {
		s.Position.SetExchangeFeeRate(s.session.ExchangeName, types.ExchangeFee{
			MakerFeeRate: s.session.MakerFeeRate,
			TakerFeeRate: s.session.TakerFeeRate,
		})
	}

	s.orderExecutor = bbgo.NewGeneralOrderExecutor(session, s.Symbol, ID, instanceID, s.Position)
	s.orderExecutor.BindEnvironment(s.Environment)
	s.orderExecutor.BindProfitStats(s.ProfitStats)
	s.orderExecutor.BindTradeStats(s.TradeStats)

	// AccountValueCalculator
	priceSolver := pricesolver.NewSimplePriceResolver(session.Markets())
	priceSolver.BindStream(session.MarketDataStream)

	s.AccountValueCalculator = bbgo.NewAccountValueCalculator(s.session, priceSolver, s.Market.QuoteCurrency)
	if err := s.AccountValueCalculator.UpdatePrices(ctx); err != nil {
		return err
	}

	// Accumulated profit report
	if bbgo.IsBackTesting {
		if s.AccumulatedProfitReport == nil {
			s.AccumulatedProfitReport = &AccumulatedProfitReport{}
		}
		s.AccumulatedProfitReport.Initialize()
		s.orderExecutor.TradeCollector().OnProfit(func(trade types.Trade, profit *types.Profit) {
			if profit == nil {
				return
			}

			s.AccumulatedProfitReport.RecordProfit(profit.Profit)
		})
		session.MarketDataStream.OnKLineClosed(types.KLineWith(s.Symbol, types.Interval1d, func(kline types.KLine) {
			s.AccumulatedProfitReport.DailyUpdate(s.TradeStats)
		}))
	}

	// For drawing
	profitSlice := floats.Slice{1., 1.}
	price, _ := session.LastPrice(s.Symbol)
	initAsset := s.CalcAssetValue(price).Float64()
	cumProfitSlice := floats.Slice{initAsset, initAsset}
	profitDollarSlice := floats.Slice{0, 0}
	cumProfitDollarSlice := floats.Slice{0, 0}

	s.orderExecutor.TradeCollector().OnTrade(func(trade types.Trade, profit fixedpoint.Value, netProfit fixedpoint.Value) {
		if bbgo.IsBackTesting {
			s.AccumulatedProfitReport.RecordTrade(trade.Fee)
		}

		// For drawing/charting
		price := trade.Price.Float64()
		if s.buyPrice > 0 {
			profitSlice.Update(price / s.buyPrice)
			cumProfitSlice.Update(s.CalcAssetValue(trade.Price).Float64())
		} else if s.sellPrice > 0 {
			profitSlice.Update(s.sellPrice / price)
			cumProfitSlice.Update(s.CalcAssetValue(trade.Price).Float64())
		}
		profitDollarSlice.Update(profit.Float64())
		cumProfitDollarSlice.Update(profitDollarSlice.Sum())
		if s.Position.IsDust(trade.Price) {
			s.buyPrice = 0
			s.sellPrice = 0
			s.highestPrice = 0
			s.lowestPrice = 0
		} else if s.Position.IsLong() {
			s.buyPrice = price
			s.sellPrice = 0
			s.highestPrice = s.buyPrice
			s.lowestPrice = 0
		} else {
			s.sellPrice = price
			s.buyPrice = 0
			s.highestPrice = 0
			s.lowestPrice = s.sellPrice
		}
	})

	s.InitDrawCommands(&profitSlice, &cumProfitSlice, &cumProfitDollarSlice)

	s.orderExecutor.TradeCollector().OnPositionUpdate(func(position *types.Position) {
		bbgo.Sync(ctx, s)
	})
	s.orderExecutor.Bind()
	s.activeOrders = bbgo.NewActiveOrderBook(s.Symbol)

	kLineStore, _ := s.session.MarketDataStore(s.Symbol)
	// window = 2 means day-to-day return, previousClose/currentClose -1
	// delay = false means use open/close-1 as D0 return (default)
	// delay = true means use open/close-1 as 10 return
	s.nrr = &NRR{IntervalWindow: types.IntervalWindow{Interval: s.Interval, Window: 2}, RankingWindow: s.Window, delay: true}
	s.nrr.BindK(s.session.MarketDataStream, s.Symbol, s.nrr.Interval)
	if klines, ok := kLineStore.KLinesOfInterval(s.nrr.Interval); ok {
		s.nrr.LoadK((*klines)[0:])
	}

	s.session.MarketDataStream.OnKLineClosed(types.KLineWith(s.Symbol, s.Interval, func(kline types.KLine) {
		alphaNrr := fixedpoint.NewFromFloat(s.nrr.RankedValues.Index(1))

		// alpha-weighted inventory and cash
		targetBase := s.calculatePosition(kline, alphaNrr)
		diffQty := targetBase.Sub(s.Position.Base)
		log.Info(alphaNrr.Float64(), s.Position.Base, diffQty.Float64())

		if err := s.orderExecutor.CancelOrders(ctx); err != nil {
			log.WithError(err).Errorf("cancel order error")
		}

		if diffQty.Sign() > 0 {
			s.placeOrders(ctx, diffQty, kline)
		} else if diffQty.Sign() < 0 {
			s.placeOrders(ctx, diffQty, kline)
		}
	}))

	// 设置默认价差
	if s.BidSpread.IsZero() {
		s.BidSpread = fixedpoint.NewFromFloat(0.0001)
	}

	if s.AskSpread.IsZero() {
		s.AskSpread = fixedpoint.NewFromFloat(0.0001)
	}

	bbgo.OnShutdown(ctx, func(ctx context.Context, wg *sync.WaitGroup) {
		defer wg.Done()
		// Output accumulated profit report
		if bbgo.IsBackTesting {
			defer s.AccumulatedProfitReport.Output(s.Symbol)

			if s.DrawGraph {
				if err := s.Draw(&profitSlice, &cumProfitSlice); err != nil {
					log.WithError(err).Errorf("cannot draw graph")
				}
			}
		} else {
			close(s.stopC)
		}
		_, _ = fmt.Fprintln(os.Stderr, s.TradeStats.String())
		_ = s.orderExecutor.GracefulCancel(ctx)
	})
	return nil
}

func (s *Strategy) CalcAssetValue(price fixedpoint.Value) fixedpoint.Value {
	balances := s.session.GetAccount().Balances()
	return balances[s.Market.BaseCurrency].Total().Mul(price).Add(balances[s.Market.QuoteCurrency].Total())
}

func (s *Strategy) placeOrders(ctx context.Context, diffQty fixedpoint.Value, kline types.KLine) {
	// 根据市场深度动态调整价差
	orderBook := s.session.MarketDataStream.GetBook()
	spread := calculateDynamicSpread(orderBook)

	if diffQty.Sign() > 0 {
		// 分批买入，避免冲击市场
		chunks := splitOrderIntoChunks(diffQty, 3) // 将订单分成3份
		for _, qty := range chunks {
			bidPrice := kline.Close.Mul(fixedpoint.One.Sub(spread))
			// ... 下单逻辑
		}
	} else if diffQty.Sign() < 0 {
		// 分批卖出
		chunks := splitOrderIntoChunks(diffQty.Abs(), 3)
		for _, qty := range chunks {
			askPrice := kline.Close.Mul(fixedpoint.One.Add(spread))
			// ... 下单逻辑
		}
	}
}

func (s *Strategy) calculatePosition(kline types.KLine, alphaNrr fixedpoint.Value) fixedpoint.Value {
	// 基础仓位
	basePosition := s.QuantityOrAmount.CalculateQuantity(kline.Close)

	// 根据趋势强度调整仓位
	trendStrength := calculateTrendStrength() // 计算趋势强度

	// 根据波动率调整仓位
	volatility := calculateVolatility() // 计算波动率

	// 动态调整最终仓位
	targetBase := basePosition.Mul(alphaNrr).
		Mul(fixedpoint.NewFromFloat(trendStrength)).
		Mul(fixedpoint.NewFromFloat(volatility))

	return targetBase
}

func (s *Strategy) checkStopPrice(ctx context.Context, currentPrice fixedpoint.Value) {
	if s.Position.IsLong() {
		// 止损价格
		stopPrice := s.Position.AverageCost.Mul(fixedpoint.One.Sub(s.StopLoss))
		if currentPrice.Compare(stopPrice) <= 0 {
			_ = s.orderExecutor.ClosePosition(ctx, fixedpoint.One)
			return
		}

		// 止盈价格
		profitPrice := s.Position.AverageCost.Mul(fixedpoint.One.Add(s.TakeProfit))
		if currentPrice.Compare(profitPrice) >= 0 {
			_ = s.orderExecutor.ClosePosition(ctx, fixedpoint.One)
			return
		}
	}
	// 做空方向类似...
}

func (s *Strategy) checkRiskLimits() bool {
	// 检查回撤
	if s.calculateDrawdown() > s.MaxDrawdown {
		return false
	}

	// 检查每日亏损
	if s.calculateDailyPnL() < s.DailyLossLimit.Neg() {
		return false
	}

	return true
}
