package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/protobuf-hex-display/json"
	"github.com/lightninglabs/protobuf-hex-display/jsonpb"
	"github.com/lightninglabs/protobuf-hex-display/proto"

	"github.com/btcsuite/btcutil"

	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

var (
	// Define route independent max routing fees. We have currently no way
	// to get a reliable estimate of the routing fees. Best we can do is
	// the minimum routing fees, which is not very indicative.
	maxRoutingFeeBase = btcutil.Amount(10)

	maxRoutingFeeRate = int64(20000)

	defaultSwapWaitTime = 30 * time.Minute

	// maxMsgRecvSize is the largest message our client will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)
)

func printJSON(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	var out bytes.Buffer
	err = json.Indent(&out, b, "", "\t")
	if err != nil {
		fatal(err)
	}
	out.WriteString("\n")
	_, _ = out.WriteTo(os.Stdout)
}

func printRespJSON(resp proto.Message) {
	jsonMarshaler := &jsonpb.Marshaler{
		OrigName:     true,
		EmitDefaults: true,
		Indent:       "    ",
	}

	jsonStr, err := jsonMarshaler.MarshalToString(resp)
	if err != nil {
		fmt.Println("unable to decode response: ", err)
		return
	}

	fmt.Println(jsonStr)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[loop] %v\n", err)
	os.Exit(1)
}

func main() {
	app := cli.NewApp()

	app.Version = loop.Version()
	app.Name = "loop"
	app.Usage = "control plane for your loopd"
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: "localhost:11010",
			Usage: "loopd daemon address host:port",
		},
	}
	app.Commands = []cli.Command{
		loopOutCommand, loopInCommand, termsCommand,
		monitorCommand, quoteCommand, listAuthCommand,
		listSwapsCommand, swapInfoCommand,
	}

	err := app.Run(os.Args)
	if err != nil {
		fatal(err)
	}
}

func getClient(ctx *cli.Context) (looprpc.SwapClientClient, func(), error) {
	rpcServer := ctx.GlobalString("rpcserver")
	conn, err := getClientConn(rpcServer)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { conn.Close() }

	loopClient := looprpc.NewSwapClientClient(conn)
	return loopClient, cleanup, nil
}

func getMaxRoutingFee(amt btcutil.Amount) btcutil.Amount {
	return swap.CalcFee(amt, maxRoutingFeeBase, maxRoutingFeeRate)
}

type limits struct {
	maxSwapRoutingFee   *btcutil.Amount
	maxPrepayRoutingFee *btcutil.Amount
	maxMinerFee         btcutil.Amount
	maxSwapFee          btcutil.Amount
	maxPrepayAmt        *btcutil.Amount
}

func getLimits(amt btcutil.Amount, quote *looprpc.QuoteResponse) *limits {
	maxSwapRoutingFee := getMaxRoutingFee(amt)
	maxPrepayRoutingFee := getMaxRoutingFee(btcutil.Amount(
		quote.PrepayAmt,
	))
	maxPrepayAmt := btcutil.Amount(quote.PrepayAmt)

	return &limits{
		maxSwapRoutingFee:   &maxSwapRoutingFee,
		maxPrepayRoutingFee: &maxPrepayRoutingFee,

		// Apply a multiplier to the estimated miner fee, to not get
		// the swap canceled because fees increased in the mean time.
		maxMinerFee: btcutil.Amount(quote.MinerFee) * 100,

		maxSwapFee:   btcutil.Amount(quote.SwapFee),
		maxPrepayAmt: &maxPrepayAmt,
	}
}

func displayLimits(swapType swap.Type, amt, minerFees btcutil.Amount, l *limits,
	externalHtlc bool, warning string) error {

	totalSuccessMax := l.maxMinerFee + l.maxSwapFee
	if l.maxSwapRoutingFee != nil {
		totalSuccessMax += *l.maxSwapRoutingFee
	}
	if l.maxPrepayRoutingFee != nil {
		totalSuccessMax += *l.maxPrepayRoutingFee
	}

	if swapType == swap.TypeIn && externalHtlc {
		fmt.Printf("On-chain fee for external loop in is not " +
			"included.\nSufficient fees will need to be paid " +
			"when constructing the transaction in the external " +
			"wallet.\n\n")
	}

	fmt.Printf("Max swap fees for %d sat Loop %v: %d sat\n", amt, swapType,
		totalSuccessMax)

	if warning != "" {
		fmt.Println(warning)
	}

	fmt.Printf("CONTINUE SWAP? (y/n), expand fee detail (x): ")

	var answer string
	fmt.Scanln(&answer)

	switch answer {
	case "y":
		return nil
	case "x":
		fmt.Println()
		f := "%-36s %d sat\n"

		switch swapType {
		case swap.TypeOut:
			fmt.Printf(f, "Estimated on-chain sweep fee:",
				minerFees)
			fmt.Printf(f, "Max on-chain sweep fee:", l.maxMinerFee)

		case swap.TypeIn:
			if !externalHtlc {
				fmt.Printf(f, "Estimated on-chain HTLC fee:",
					minerFees)
			}
		}

		if l.maxSwapRoutingFee != nil {
			fmt.Printf(f, "Max off-chain swap routing fee:",
				*l.maxSwapRoutingFee)
		}

		if l.maxPrepayAmt != nil {
			fmt.Printf(f, "Max no show penalty (prepay):",
				*l.maxPrepayAmt)
		}
		if l.maxPrepayRoutingFee != nil {
			fmt.Printf(f, "Max off-chain prepay routing fee:",
				*l.maxPrepayRoutingFee)
		}
		fmt.Printf(f, "Max swap fee:", l.maxSwapFee)

		fmt.Printf("CONTINUE SWAP? (y/n): ")
		fmt.Scanln(&answer)
		if answer == "y" {
			return nil
		}
	}

	return errors.New("swap canceled")
}

func parseAmt(text string) (btcutil.Amount, error) {
	amtInt64, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amt value")
	}
	return btcutil.Amount(amtInt64), nil
}

func logSwap(swap *looprpc.SwapStatus) {
	if swap.Type == looprpc.SwapType_LOOP_OUT {
		fmt.Printf("%v %v %v %v - %v",
			time.Unix(0, swap.LastUpdateTime).Format(time.RFC3339),
			swap.Type, swap.State, btcutil.Amount(swap.Amt),
			swap.HtlcAddressP2Wsh,
		)
	} else {
		fmt.Printf("%v %v %v %v -",
			time.Unix(0, swap.LastUpdateTime).Format(time.RFC3339),
			swap.Type, swap.State, btcutil.Amount(swap.Amt))
		if swap.HtlcAddressP2Wsh != "" {
			fmt.Printf(" P2WSH: %v", swap.HtlcAddressP2Wsh)
		}

		if swap.HtlcAddressNp2Wsh != "" {
			fmt.Printf(" NP2WSH: %v", swap.HtlcAddressNp2Wsh)
		}
	}

	if swap.State != looprpc.SwapState_INITIATED &&
		swap.State != looprpc.SwapState_HTLC_PUBLISHED &&
		swap.State != looprpc.SwapState_PREIMAGE_REVEALED {

		fmt.Printf(" (cost: server %v, onchain %v, offchain %v)",
			swap.CostServer, swap.CostOnchain, swap.CostOffchain,
		)
	}

	fmt.Println()
}

func getClientConn(address string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithInsecure(),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}

	conn, err := grpc.Dial(address, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %v", err)
	}

	return conn, nil
}
