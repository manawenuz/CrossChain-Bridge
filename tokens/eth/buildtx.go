package eth

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/anyswap/CrossChain-Bridge/common"
	"github.com/anyswap/CrossChain-Bridge/log"
	"github.com/anyswap/CrossChain-Bridge/params"
	"github.com/anyswap/CrossChain-Bridge/tokens"
	"github.com/anyswap/CrossChain-Bridge/types"
)

var (
	retryRPCCount    = 3
	retryRPCInterval = 1 * time.Second

	defReserveGasFee = big.NewInt(1e16) // 0.01 ETH
	defGasLimit      = uint64(90000)
)

// BuildRawTransaction build raw tx
func (b *Bridge) BuildRawTransaction(args *tokens.BuildTxArgs) (rawTx interface{}, err error) {
	var input []byte
	var tokenCfg *tokens.TokenConfig
	if args.Input == nil {
		if args.SwapType != tokens.NoSwapType {
			pairID := args.PairID
			tokenCfg = b.GetTokenConfig(pairID)
			if tokenCfg == nil {
				return nil, tokens.ErrUnknownPairID
			}
			if args.From == "" {
				args.From = tokenCfg.DcrmAddress // from
			}
		}
		switch args.SwapType {
		case tokens.SwapinType:
			if b.IsSrc {
				return nil, tokens.ErrBuildSwapTxInWrongEndpoint
			}
			err = b.buildSwapinTxInput(args)
			if err != nil {
				return nil, err
			}
			input = *args.Input
		case tokens.SwapoutType:
			if !b.IsSrc {
				return nil, tokens.ErrBuildSwapTxInWrongEndpoint
			}
			if tokenCfg.IsErc20() {
				err = b.buildErc20SwapoutTxInput(args)
				if err != nil {
					return nil, err
				}
				input = *args.Input
			} else {
				args.To = args.Bind
				input = []byte(tokens.UnlockMemoPrefix + args.SwapID)
			}
		}
	} else {
		input = *args.Input
		if args.SwapType != tokens.NoSwapType {
			return nil, fmt.Errorf("forbid build raw swap tx with input data")
		}
	}

	extra, err := b.setDefaults(args)
	if err != nil {
		return nil, err
	}

	return b.buildTx(args, extra, input)
}

func (b *Bridge) buildTx(args *tokens.BuildTxArgs, extra *tokens.EthExtraArgs, input []byte) (rawTx interface{}, err error) {
	var (
		to       = common.HexToAddress(args.To)
		value    = args.Value
		nonce    = *extra.Nonce
		gasLimit = *extra.Gas
		gasPrice = extra.GasPrice
	)

	if args.SwapType == tokens.SwapoutType {
		pairID := args.PairID
		tokenCfg := b.GetTokenConfig(pairID)
		if tokenCfg == nil {
			return nil, tokens.ErrUnknownPairID
		}
		if !tokenCfg.IsErc20() {
			value = tokens.CalcSwappedValue(pairID, args.OriginValue, false)
		}
	}

	if args.SwapType != tokens.NoSwapType {
		args.Identifier = params.GetIdentifier()
	}

	gasFee := defReserveGasFee
	if args.SwapType == tokens.NoSwapType {
		gasFee = new(big.Int).Mul(gasPrice, new(big.Int).SetUint64(gasLimit))
	}

	err = b.checkCoinBalance(args.From, value, gasFee)
	if err != nil {
		return nil, err
	}

	rawTx = types.NewTransaction(nonce, to, value, gasLimit, gasPrice, input)

	log.Trace("build raw tx", "pairID", args.PairID, "identifier", args.Identifier,
		"swapID", args.SwapID, "swapType", args.SwapType,
		"bind", args.Bind, "originValue", args.OriginValue,
		"from", args.From, "to", to.String(), "value", value, "nonce", nonce,
		"gasLimit", gasLimit, "gasPrice", gasPrice, "data", common.ToHex(input))

	return rawTx, nil
}

func (b *Bridge) setDefaults(args *tokens.BuildTxArgs) (extra *tokens.EthExtraArgs, err error) {
	if args.Value == nil {
		args.Value = new(big.Int)
	}
	if args.Extra == nil || args.Extra.EthExtra == nil {
		extra = &tokens.EthExtraArgs{}
		args.Extra = &tokens.AllExtras{EthExtra: extra}
	} else {
		extra = args.Extra.EthExtra
	}
	if extra.GasPrice == nil {
		extra.GasPrice, err = b.getGasPrice()
		if err != nil {
			return nil, err
		}
		if args.SwapType != tokens.NoSwapType {
			pairID := args.PairID
			tokenCfg := b.GetTokenConfig(pairID)
			if tokenCfg == nil {
				return nil, tokens.ErrUnknownPairID
			}
			addPercent := tokenCfg.PlusGasPricePercentage
			if addPercent > 0 {
				extra.GasPrice.Mul(extra.GasPrice, big.NewInt(int64(100+addPercent)))
				extra.GasPrice.Div(extra.GasPrice, big.NewInt(100))
			}
		}
	}
	if extra.Nonce == nil {
		extra.Nonce, err = b.getAccountNonce(args.PairID, args.From, args.SwapType)
		if err != nil {
			return nil, err
		}
	}
	if extra.Gas == nil {
		extra.Gas = new(uint64)
		*extra.Gas = b.getDefaultGasLimit(args.PairID)
	}
	return extra, nil
}

func (b *Bridge) getBalance(account string) (balance *big.Int, err error) {
	for i := 0; i < retryRPCCount; i++ {
		balance, err = b.GetBalance(account)
		if err == nil {
			return balance, nil
		}
		time.Sleep(retryRPCInterval)
	}
	return nil, err
}

func (b *Bridge) getErc20Balance(erc20Addr, account string) (balance *big.Int, err error) {
	for i := 0; i < retryRPCCount; i++ {
		balance, err = b.GetErc20Balance(erc20Addr, account)
		if err == nil {
			return balance, nil
		}
		time.Sleep(retryRPCInterval)
	}
	return nil, err
}

func (b *Bridge) getDefaultGasLimit(pairID string) (gasLimit uint64) {
	tokenCfg := b.GetTokenConfig(pairID)
	if tokenCfg != nil {
		gasLimit = tokenCfg.DefaultGasLimit
	}
	if gasLimit == 0 {
		gasLimit = defGasLimit
	}
	return gasLimit
}

func (b *Bridge) getGasPrice() (price *big.Int, err error) {
	for i := 0; i < retryRPCCount; i++ {
		price, err = b.SuggestPrice()
		if err == nil {
			return price, nil
		}
		time.Sleep(retryRPCInterval)
	}
	return nil, err
}

func (b *Bridge) getAccountNonce(pairID, from string, swapType tokens.SwapType) (nonceptr *uint64, err error) {
	var nonce uint64
	for i := 0; i < retryRPCCount; i++ {
		nonce, err = b.GetPoolNonce(from, "pending")
		if err == nil {
			break
		}
		time.Sleep(retryRPCInterval)
	}
	if err != nil {
		return nil, err
	}
	if swapType != tokens.NoSwapType {
		tokenCfg := b.GetTokenConfig(pairID)
		if tokenCfg != nil && from == tokenCfg.DcrmAddress {
			nonce = b.AdjustNonce(pairID, nonce)
		}
	}
	return &nonce, nil
}

// build input for calling `Swapin(bytes32 txhash, address account, uint256 amount)`
func (b *Bridge) buildSwapinTxInput(args *tokens.BuildTxArgs) error {
	pairID := args.PairID
	funcHash := getSwapinFuncHash()
	txHash := common.HexToHash(args.SwapID)
	address := common.HexToAddress(args.Bind)
	if address == (common.Address{}) || !common.IsHexAddress(args.Bind) {
		log.Warn("swapin to wrong address", "address", args.Bind)
		return errors.New("can not swapin to empty or invalid address")
	}
	amount := tokens.CalcSwappedValue(pairID, args.OriginValue, true)

	input := PackDataWithFuncHash(funcHash, txHash, address, amount)
	args.Input = &input // input

	token := b.GetTokenConfig(pairID)
	if token == nil {
		return tokens.ErrUnknownPairID
	}
	args.To = token.ContractAddress // to
	return nil
}

func (b *Bridge) buildErc20SwapoutTxInput(args *tokens.BuildTxArgs) (err error) {
	pairID := args.PairID
	funcHash := erc20CodeParts["transfer"]
	address := common.HexToAddress(args.Bind)
	if address == (common.Address{}) || !common.IsHexAddress(args.Bind) {
		log.Warn("swapout to wrong address", "address", args.Bind)
		return errors.New("can not swapout to empty or invalid address")
	}
	amount := tokens.CalcSwappedValue(pairID, args.OriginValue, false)

	input := PackDataWithFuncHash(funcHash, address, amount)
	args.Input = &input // input

	token := b.GetTokenConfig(pairID)
	if token == nil {
		return tokens.ErrUnknownPairID
	}
	args.To = token.ContractAddress // to
	return b.checkTokenBalance(token.ContractAddress, token.DcrmAddress, amount)
}

func (b *Bridge) checkTokenBalance(token, from string, value *big.Int) (err error) {
	var balance *big.Int
	for i := 0; i < retryRPCCount; i++ {
		balance, err = b.GetErc20Balance(token, from)
		if err == nil {
			break
		}
		time.Sleep(retryRPCInterval)
	}
	if err != nil {
		return err
	}
	if balance.Cmp(value) < 0 {
		return fmt.Errorf("not enough token balance, have %v, need %v", balance, value)
	}
	return nil
}

func (b *Bridge) checkCoinBalance(from string, value, gasFee *big.Int) (err error) {
	balance, err := b.getBalance(from)
	if err != nil {
		log.Warn("get balance error", "from", from, "err", err)
		return fmt.Errorf("get balance of %v error: %v", from, err)
	}
	needValue := big.NewInt(0)
	if value != nil && value.Sign() > 0 {
		needValue.Add(needValue, value)
	}
	if gasFee != nil && gasFee.Sign() > 0 {
		needValue.Add(needValue, gasFee)
	}
	if balance.Cmp(needValue) < 0 {
		return fmt.Errorf("not enough coin balance, have %v, need %v plus gas fee %v", balance, value, gasFee)
	}
	return nil
}
