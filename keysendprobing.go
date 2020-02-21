package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/record"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"gopkg.in/macaroon.v2"
	"io/ioutil"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

const (
	//
	//aliceRpc          = "127.0.0.1:10009"
	//aliceTlsPath      = "../.lnd/tls.cert"
	//aliceMacaroonPath = "../.lnd/data/chain/bitcoin/mainnet/admin.macaroon"
	////
	//aliceRpc          = "127.0.0.1:10009"
	//aliceTlsPath      = "/Users/kon/Library/Application Support/Donner/daemon/data/tls.cert"
	//aliceMacaroonPath = "/Users/kon/Library/Application Support/Donner/daemon/data/lnd/chain/bitcoin/mainnet/admin.macaroon"


	totalSatsPaidKey     = "totalSatsPaid"
	totalPaymentsMadeKey = "totalPaymentsMade"
)

type PayRequest struct {
	id      uint32
	message string
	node    *lnrpc.LightningNode
}
type PayResponse struct {
	id   uint32
	node *lnrpc.LightningNode
	res  *lnrpc.SendResponse
}

type ErrorResponse struct {
	id   uint32
	node *lnrpc.LightningNode
	err  error
}

type ProbingService struct {
	totalSpendAmount int64
	satsPerPayment   int64
	standardMessage  string
	payReqChan       chan *PayRequest
	payResChan       chan *PayResponse
	errResChan       chan *ErrorResponse
	inflightChan	 chan struct{}
	lnd              lnrpc.LightningClient
	satsMap          sync.Map
}


func main() {
	if err := doProbing(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return
	}
}

func doProbing() error{
	var aliceRpc, aliceTlsPath, aliceMacaroonPath string
	var totalSpend, satPerPayment, inflight int64
	var standardMessage string
	flag.StringVar(&standardMessage, "message", "hi", "default message to send")
	flag.StringVar(&aliceRpc, "rpc", "localhost:10009", "lnd rpc host")
	flag.StringVar(&aliceTlsPath, "tls", "/home/lnd/.lnd/tls.cert","tls.cert location")
	flag.StringVar(&aliceMacaroonPath, "macaroon", "/home/lnd/.lnd/data/chain/bitcoin/mainnet/admin.macaroon", "location of admin.macaroon")
	flag.Int64Var(&satPerPayment, "payment_amount", 1, "amount of sats per payment")
	flag.Int64Var(&totalSpend, "spend_amount", 0, "total amount of sats to spend")
	flag.Int64Var(&inflight, "inflight", 10, "max payments inflight")
	flag.Parse()


	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	errChan := make(chan error)
	termChan := make(chan os.Signal)
	finishChan := make(chan struct{})
	aliceCon, err := getClientConnection(ctx, aliceRpc, aliceTlsPath, aliceMacaroonPath)
	if err != nil {
		return err
	}
	defer aliceCon.Close()

	aliceLnd := lnrpc.NewLightningClient(aliceCon)
	getinfo, err := aliceLnd.GetInfo(ctx, &lnrpc.GetInfoRequest{})
	if err != nil {
		return err
	}
	fmt.Printf("%v \n", getinfo)


	totalSpend = totalSpend*1000
	probingService := &ProbingService{
		totalSpendAmount: totalSpend,
		satsPerPayment:   satPerPayment,
		standardMessage:  standardMessage,
		satsMap:          sync.Map{},
		payReqChan:       make(chan *PayRequest),
		payResChan:       make(chan *PayResponse),
		errResChan:       make(chan *ErrorResponse),
		inflightChan:	  make(chan struct{}, inflight),
		lnd:              aliceLnd,
	}
	probingService.writeMap(totalPaymentsMadeKey, 0)
	probingService.writeMap(totalSatsPaidKey, 0)
	go probingService.paymentHandler(ctx)
	go probingService.responseHandler(errChan, finishChan)
	go probingService.requestHandler(ctx, errChan)

	signal.Notify(termChan, syscall.SIGINT, syscall.SIGTERM)
	for {
		select {
		case _ = <-finishChan:
			totalSatsPaid, err := probingService.readMap(totalSatsPaidKey)
			if err != nil{
				return err
			}
			totalPaymentsMade, err := probingService.readMap(totalPaymentsMadeKey)
			if err != nil{
				return err
			}
			fmt.Printf("Finished: total msats spent %d to %d nodes\n", totalSatsPaid, totalPaymentsMade)
			return nil
		case _ = <-termChan:
			totalSatsPaid, err := probingService.readMap(totalSatsPaidKey)
			if err != nil{
				return err
			}
			totalPaymentsMade, err := probingService.readMap(totalPaymentsMadeKey)
			if err != nil{
				return err
			}
			fmt.Printf("Canceled: total msats spent %d to %d nodes\n", totalSatsPaid, totalPaymentsMade)
			return nil
		case errC := <- errChan:
			totalSatsPaid, err := probingService.readMap(totalSatsPaidKey)
			if err != nil{
				return err
			}
			totalPaymentsMade, err := probingService.readMap(totalPaymentsMadeKey)
			if err != nil{
				return err
			}

			fmt.Printf("Error: %s total msats spent %d to %d nodes\n", errC.Error(), totalSatsPaid, totalPaymentsMade)
			return err
		}
	}
}

func (p *ProbingService) paymentHandler(ctx context.Context) {
	for {
		select {
		case req := <-p.payReqChan:
			if req.id == 0 {
				p.payResChan <- &PayResponse{
					id:   0,
					node: nil,
					res:  nil,
				}
				return
			}
			p.inflightChan <-struct{}{}
			go func(){
				res, err := p.sendPayment(ctx, p.lnd, req.node, p.standardMessage)
				if err != nil {
					_ = <- p.inflightChan
					p.errResChan <- &ErrorResponse{
						id:   req.id,
						node: req.node,
						err:  err,
					}
					return
				}
				if res.PaymentError != "" {
					_ = <- p.inflightChan
					p.errResChan <- &ErrorResponse{
						id:   req.id,
						node: req.node,
						err:  errors.New(res.PaymentError),
					}
					return
				}
				_ = <- p.inflightChan
				p.payResChan <- &PayResponse{
					id:   req.id,
					node: req.node,
					res:  res,
				}
			}()

		}
	}

}

func (p *ProbingService) responseHandler(errChan chan error, finishChan chan struct{}) {
	for {
		select {
		case res := <-p.payResChan:
			if res.id == 0{
				close(finishChan)
				return
			}
			paymentsMade, err := p.readMap(totalPaymentsMadeKey)
			if err != nil {
				errChan <- err
				return
			}
			totalSatsPaid, err := p.readMap(totalSatsPaidKey)
			if err != nil {
				errChan <- err
				return
			}
			p.writeMap(totalPaymentsMadeKey, paymentsMade+1)
			p.writeMap(totalSatsPaidKey, totalSatsPaid + res.res.PaymentRoute.TotalAmtMsat)
			fmt.Printf("%v paid %v msats (fee: %d) to %s \n", res.id, res.res.PaymentRoute.TotalAmtMsat, res.res.PaymentRoute.TotalFeesMsat, res.node.PubKey)
			fmt.Printf("%v details: %v \n", res.id, res.res)
		case err := <-p.errResChan:
			fmt.Printf("%d error tried paying to:%s msg: %s \n", err.id, err.node.PubKey, err.err.Error())
		}
	}
}

func (p *ProbingService) requestHandler(ctx context.Context, errChan chan error) {
	var nodeList []*lnrpc.LightningNode
	graph, err := p.lnd.DescribeGraph(ctx, &lnrpc.ChannelGraphRequest{IncludeUnannounced: false})
	if err != nil {
		errChan <- err
		return
	}
	if graph == nil {
		errChan <- errors.New("no nodes found")
		return
	}
	for _, node := range graph.Nodes {
		if node.Features == nil {
			continue
		}
		if feat, ok := node.Features[uint32(lnrpc.FeatureBit_TLV_ONION_OPT)]; ok {
			if feat.IsKnown {
				nodeList = append(nodeList, node)
			}
		}
	}
	fmt.Println("totalNodes with tlv onion featurebit = ", len(nodeList))
	for i, node := range nodeList {
		totalSatsSpent, err := p.readMap((totalSatsPaidKey))
		if err != nil {
			errChan <- err
			return
		}
		if (totalSatsSpent+ p.satsPerPayment) > p.totalSpendAmount {
			errChan <- errors.New("no more remaining sats")
			return
		}
		p.payReqChan <- &PayRequest{
			id:   uint32(i+1),
			node: node,
		}
	}
	p.payReqChan <- &PayRequest{
		id:      0,
		message: "",
		node:    nil,
	}

}

func (p *ProbingService) sendPayment(ctx context.Context, client lnrpc.LightningClient, node *lnrpc.LightningNode, message string) (*lnrpc.SendResponse, error) {
	// get phash and preimage
	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		return nil, err
	}
	pHash := preimage.Hash()
	dest, err := hex.DecodeString(node.PubKey)
	if err != nil {
		return nil, err
	}
	sendPayRequest := &lnrpc.SendRequest{
		Dest:              dest,
		Amt:               p.satsPerPayment,
		DestCustomRecords: make(map[uint64][]byte),
		PaymentHash:       pHash[:],
	}
	sendPayRequest.DestCustomRecords[record.KeySendType] = preimage[:]
	sendPayRequest.DestCustomRecords[13371337] = []byte(message)
	paymentStream, err := client.SendPayment(ctx)
	if err != nil {
		return nil, err
	}

	if err := paymentStream.Send(sendPayRequest); err != nil {
		return nil, err
	}

	resp, err := paymentStream.Recv()
	if err != nil {
		return nil, err
	}

	if err := paymentStream.CloseSend(); err != nil {
		return nil, err
	}

	if resp.PaymentError != "" {
		return nil, errors.New(resp.PaymentError)
	}

	return resp, nil
}

// gets the lnd grpc connection
func getClientConnection(ctx context.Context, address string, tlsCertPath string, macaroonPath string) (*grpc.ClientConn, error) {
	maxMsgRecvSize := grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 500)

	creds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		return nil, err
	}
	macBytes, err := ioutil.ReadFile(macaroonPath)
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, err
	}
	cred := macaroons.NewMacaroonCredential(mac)
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(),
		grpc.WithPerRPCCredentials(cred),
		grpc.WithDefaultCallOptions(maxMsgRecvSize),
	}
	conn, err := grpc.DialContext(ctx, address, opts...)
	if err != nil {
		return nil, err
	}
	return conn, nil

}

func (p *ProbingService) readMap(key string) (int64, error) {

	if val, ok := p.satsMap.Load(key); ok {
		return val.(int64), nil
	}
	return 0, errors.New("could not read map")
}
func (p *ProbingService) writeMap(key string, value int64) {
	p.satsMap.Store(key,value)
}