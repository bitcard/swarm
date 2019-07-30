// Copyright 2019 The Swarm Authors
// This file is part of the Swarm library.
//
// The Swarm library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Swarm library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Swarm library. If not, see <http://www.gnu.org/licenses/>.

package swap

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethersphere/swarm/contracts/swap"
	cswap "github.com/ethersphere/swarm/contracts/swap"
	"github.com/ethersphere/swarm/log"
	"github.com/ethersphere/swarm/p2p/protocols"
	"github.com/ethersphere/swarm/state"
)

// ErrInvalidChequeSignature indicates the signature on the cheque was invalid
var ErrInvalidChequeSignature = errors.New("invalid cheque signature")

// Swap represents the SwAP Swarm Accounting Protocol
// a peer to peer micropayment system
// A node maintains an individual balance with every peer
// Only messages which have a price will be accounted for
type Swap struct {
	api                 PublicAPI
	stateStore          state.Store          // stateStore is needed in order to keep balances across sessions
	lock                sync.RWMutex         // lock the store
	balances            map[enode.ID]int64   // map of balances for each peer
	cheques             map[enode.ID]*Cheque // map of cheques for each peer
	peers               map[enode.ID]*Peer   // map of all swap Peers
	backend             cswap.Backend        // the backend (blockchain) used
	owner               *Owner               // contract access
	params              *Params              // economic and operational parameters
	contractReference   *swap.Swap           // reference to the smart contract
	oracle              PriceOracle          // the oracle providing the ether price for honey
	paymentThreshold    int64                // balance difference required for sending cheque
	disconnectThreshold int64                // balance difference required for dropping peer
}

// Owner encapsulates information related to accessing the contract
type Owner struct {
	Contract   common.Address    // address of swap contract
	address    common.Address    // owner address
	privateKey *ecdsa.PrivateKey // private key
	publicKey  *ecdsa.PublicKey  // public key
}

// Params encapsulates param
type Params struct {
	InitialDepositAmount uint64 //
}

// NewDefaultParams returns a Params struct filled with default values
func NewDefaultParams() *Params {
	return &Params{
		InitialDepositAmount: DefaultInitialDepositAmount,
	}
}

// New - swap constructor
func New(stateStore state.Store, prvkey *ecdsa.PrivateKey, contract common.Address, backend cswap.Backend) *Swap {
	sw := &Swap{
		stateStore:          stateStore,
		balances:            make(map[enode.ID]int64),
		backend:             backend,
		cheques:             make(map[enode.ID]*Cheque),
		peers:               make(map[enode.ID]*Peer),
		params:              NewDefaultParams(),
		paymentThreshold:    DefaultPaymentThreshold,
		disconnectThreshold: DefaultDisconnectThreshold,
		contractReference:   nil,
		oracle:              NewPriceOracle(),
	}
	sw.owner = sw.createOwner(prvkey, contract)
	return sw
}

const (
	balancePrefix        = "balance_"
	sentChequePrefix     = "sent_cheque_"
	receivedChequePrefix = "received_cheque_"
)

// returns the store key for retrieving a peer's balance
func balanceKey(peer enode.ID) string {
	return balancePrefix + peer.String()
}

// returns the store key for retrieving a peer's last sent cheque
func sentChequeKey(peer enode.ID) string {
	return sentChequePrefix + peer.String()
}

// returns the store key for retrieving a peer's last received cheque
func receivedChequeKey(peer enode.ID) string {
	return receivedChequePrefix + peer.String()
}

func keyToID(key string, prefix string) enode.ID {
	return enode.HexID(key[len(prefix):])
}

// createOwner assings keys and addresses
func (s *Swap) createOwner(prvkey *ecdsa.PrivateKey, contract common.Address) *Owner {
	pubkey := &prvkey.PublicKey
	return &Owner{
		privateKey: prvkey,
		publicKey:  pubkey,
		Contract:   contract,
		address:    crypto.PubkeyToAddress(*pubkey),
	}
}

// DeploySuccess is for convenience log output
func (s *Swap) DeploySuccess() string {
	return fmt.Sprintf("contract: %s, owner: %s, deposit: %v, signer: %x", s.owner.Contract.Hex(), s.owner.address.Hex(), s.params.InitialDepositAmount, s.owner.publicKey)
}

// Add is the (sole) accounting function
// Swap implements the protocols.Balance interface
func (s *Swap) Add(amount int64, peer *protocols.Peer) (err error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	// load existing balances from the state store
	err = s.loadBalance(peer.ID())
	if err != nil && err != state.ErrNotFound {
		log.Error("error while loading balance for peer", "peer", peer.ID().String())
		return
	}

	// Check if balance with peer is over the disconnect threshold
	// It is the creditor who triggers the disconnect from a overdraft creditor,
	// thus we check for a positive value
	if s.balances[peer.ID()] >= s.disconnectThreshold {
		// if so, return error in order to abort the transfer
		disconnectMessage := fmt.Sprintf("balance for peer %s is over the disconnect threshold %v, disconnecting", peer.ID().String(), s.disconnectThreshold)
		log.Warn(disconnectMessage)
		return errors.New(disconnectMessage)
	}

	// calculate new balance
	var newBalance int64
	newBalance, err = s.updateBalance(peer.ID(), amount)
	if err != nil {
		return
	}

	// Check if balance with peer crosses the threshold
	// It is the peer with a negative balance who sends a cheque, thus we check
	// that the balance is *below* the threshold
	if newBalance <= -s.paymentThreshold {
		//if so, send cheque
		log.Warn("balance for peer went over the payment threshold, sending cheque", "peer", peer.ID().String(), "payment threshold", s.paymentThreshold)
		err = s.sendCheque(peer.ID())
		if err != nil {
			log.Error("error while sending cheque to peer", "peer", peer.ID().String(), "error", err.Error())
		} else {
			log.Info("successfully sent cheque to peer", "peer", peer.ID().String())
		}
	}

	return
}

func (s *Swap) updateBalance(peer enode.ID, amount int64) (int64, error) {
	//adjust the balance
	//if amount is negative, it will decrease, otherwise increase
	s.balances[peer] += amount
	//save the new balance to the state store
	peerBalance := s.balances[peer]
	err := s.stateStore.Put(balanceKey(peer), &peerBalance)
	if err != nil {
		log.Error("error while storing balance for peer", "peer", peer.String())
	}
	log.Debug("balance for peer after accounting", "peer", peer.String(), "balance", strconv.FormatInt(peerBalance, 10))
	return peerBalance, err
}

// loadBalance loads balances from the state store (persisted)
func (s *Swap) loadBalance(peer enode.ID) (err error) {
	var peerBalance int64
	//only load if the current instance doesn't already have this peer's
	//balance in memory
	if _, ok := s.balances[peer]; !ok {
		err = s.stateStore.Get(balanceKey(peer), &peerBalance)
		s.balances[peer] = peerBalance
	}
	return
}

// logBalance is a helper function to log the current balance of a peer
func (s *Swap) logBalance(peer *protocols.Peer) {
	err := s.loadBalance(peer.ID())
	if err != nil && err != state.ErrNotFound {
		log.Error("error while loading balance for peer", "peer", peer.String())
	} else {
		log.Info("balance for peer", "peer", peer.ID(), "balance", s.balances[peer.ID()])
	}
}

// sendCheque sends a cheque to peer
func (s *Swap) sendCheque(peer enode.ID) error {
	swapPeer := s.getPeer(peer)
	cheque, err := s.createCheque(peer)
	if err != nil {
		log.Error("error while creating cheque: %s", err.Error())
		return err
	}

	log.Info("sending cheque", "serial", cheque.ChequeParams.Serial, "amount", cheque.ChequeParams.Amount, "beneficiary", cheque.Beneficiary, "contract", cheque.Contract)
	s.cheques[peer] = cheque

	err = s.stateStore.Put(sentChequeKey(peer), &cheque)
	// TODO: error handling might be quite more complex
	if err != nil {
		log.Error("error while storing the last cheque: %s", err.Error())
		return err
	}

	emit := &EmitChequeMsg{
		Cheque: cheque,
	}

	// reset balance;
	// TODO: if sending fails it should actually be roll backed...
	s.resetBalance(peer, int64(cheque.Amount))

	err = swapPeer.Send(context.TODO(), emit)
	return err
}

// Create a Cheque structure emitted to a specific peer as a beneficiary
// The serial and amount of the cheque will depend on the last cheque and current balance for this peer
// The cheque will be signed and point to the issuer's contract
func (s *Swap) createCheque(peer enode.ID) (*Cheque, error) {
	var cheque *Cheque
	var err error

	swapPeer := s.getPeer(peer)
	beneficiary := swapPeer.beneficiary

	peerBalance := s.balances[peer]
	// the balance should be negative here, we take the absolute value:
	honey := uint64(-peerBalance)

	// convert honey to ETH
	var amount uint64
	amount, err = s.oracle.GetPrice(honey)
	if err != nil {
		log.Error("error getting price from oracle", "err", err)
		return nil, err
	}

	// we need to ignore the error check when loading from the StateStore,
	// as an error might indicate that there is no existing cheque, which
	// could mean it's the first interaction, which is absolutely valid
	_ = s.loadLastSentCheque(peer)
	lastCheque := s.cheques[peer]

	if lastCheque == nil {
		cheque = &Cheque{
			ChequeParams: ChequeParams{
				Serial: uint64(1),
				Amount: uint64(amount),
			},
		}
	} else {
		cheque = &Cheque{
			ChequeParams: ChequeParams{
				Serial: lastCheque.Serial + 1,
				Amount: lastCheque.Amount + uint64(amount),
			},
		}
	}
	cheque.ChequeParams.Timeout = defaultCashInDelay
	cheque.ChequeParams.Contract = s.owner.Contract
	cheque.ChequeParams.Honey = uint64(honey)
	cheque.Beneficiary = beneficiary

	cheque.Sig, err = s.signContent(cheque)

	return cheque, err
}

// Balance returns the balance for a given peer
func (s *Swap) Balance(peer enode.ID) (int64, error) {
	var err error
	// check the balance in memory
	peerBalance, ok := s.balances[peer]
	// if not present, check in disk
	if !ok {
		err = s.stateStore.Get(balanceKey(peer), &peerBalance)
	}
	return peerBalance, err
}

// Balances returns the balances for all known SWAP peers
func (s *Swap) Balances() (map[enode.ID]int64, error) {
	balances := make(map[enode.ID]int64)

	// get list of all known SWAP peers to have a balance
	swapPeers, err := s.BalancePeers()
	if err != nil {
		return nil, err
	}

	// get balance for list of peers
	for _, peer := range swapPeers {
		peerBalance, err := s.Balance(peer)
		if err != nil {
			return nil, err
		}
		balances[peer] = peerBalance
	}

	return balances, nil
}

// BalancePeers returns a list of every peer known to have a balance set through SWAP
func (s *Swap) BalancePeers() (peers []enode.ID, err error) {
	knownPeers := make(map[enode.ID]bool)

	// add in-memory balance peers and mark as present
	for peerID := range s.balances {
		peers = append(peers, peerID)
		knownPeers[peerID] = true
	}

	// get balance keys from store
	storeBalancePeers, err := s.stateStore.Keys(balancePrefix)
	if err != nil {
		return nil, err
	}

	// add balance peer to result if not present in memory
	for _, storeBalancePeer := range storeBalancePeers {
		// take balance key and turn into node ID
		peerID := keyToID(storeBalancePeer, balancePrefix)
		if _, peerExists := knownPeers[peerID]; !peerExists {
			peers = append(peers, peerID)
		}
	}

	return peers, nil
}

// loadLastSentCheque loads the last cheque for a peer from the state store (persisted)
func (s *Swap) loadLastSentCheque(peer enode.ID) (err error) {
	//only load if the current instance doesn't already have this peer's
	//last cheque in memory
	var cheque *Cheque
	if _, ok := s.cheques[peer]; !ok {
		err = s.stateStore.Get(sentChequeKey(peer), &cheque)
		s.cheques[peer] = cheque
	}
	return
}

// saveLastReceivedCheque loads the last received cheque for peer
func (s *Swap) loadLastReceivedCheque(peer enode.ID) (cheque *Cheque) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.stateStore.Get(receivedChequeKey(peer), &cheque)
	return
}

// saveLastReceivedCheque saves cheque as the last received cheque for peer
func (s *Swap) saveLastReceivedCheque(peer enode.ID, cheque *Cheque) error {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.stateStore.Put(receivedChequeKey(peer), cheque)
}

// Close cleans up swap
func (s *Swap) Close() {
	s.stateStore.Close()
}

// resetBalance is called:
// * for the creditor: on cheque receival
// * for the debitor: on confirmation receival
func (s *Swap) resetBalance(peerID enode.ID, amount int64) {
	log.Info("resetting balance for peer", "peer", peerID.String(), "amount", amount)
	s.updateBalance(peerID, amount)
}

// signContent signs the cheque with the owners private key
func (s *Swap) signContent(cheque *Cheque) ([]byte, error) {
	return cheque.Sign(s.owner.privateKey)
}

// GetParams returns contract parameters (Bin, ABI) from the contract
func (s *Swap) GetParams() *swap.Params {
	return s.contractReference.ContractParams()
}

// Deploy deploys a new swap contract
func (s *Swap) Deploy(ctx context.Context, backend swap.Backend, path string) error {
	// TODO: What to do if the contract is already deployed?
	return s.deploy(ctx, backend, path)
}

// verifyContract checks if the bytecode found at address matches the expected bytecode
func (s *Swap) verifyContract(ctx context.Context, address common.Address) error {
	swap, err := swap.InstanceAt(address, s.backend)
	if err != nil {
		return err
	}

	return swap.ValidateCode(ctx, s.backend, address)
}

// getContractOwner retrieve the owner of the chequebook at address from the blockchain
func (s *Swap) getContractOwner(ctx context.Context, address common.Address) (common.Address, error) {
	swap, err := swap.InstanceAt(address, s.backend)
	if err != nil {
		return common.Address{}, err
	}

	return swap.Instance.Issuer(nil)
}

// deploy deploys the Swap contract
func (s *Swap) deploy(ctx context.Context, backend swap.Backend, path string) error {
	opts := bind.NewKeyedTransactor(s.owner.privateKey)
	// initial topup value
	opts.Value = big.NewInt(int64(s.params.InitialDepositAmount))
	opts.Context = ctx

	log.Info("deploying new swap", "owner", opts.From.Hex())
	address, err := s.deployLoop(opts, backend, s.owner.address, defaultHarddepositTimeoutDuration)
	if err != nil {
		log.Error("unable to deploy swap", "error", err)
		return err
	}
	s.owner.Contract = address
	log.Info("swap deployed", "address", address.Hex(), "owner", opts.From.Hex())

	return err
}

// deployLoop repeatedly tries to deploy the swap contract .
func (s *Swap) deployLoop(opts *bind.TransactOpts, backend swap.Backend, owner common.Address, defaultHarddepositTimeoutDuration time.Duration) (addr common.Address, err error) {
	var tx *types.Transaction
	for try := 0; try < deployRetries; try++ {
		if try > 0 {
			time.Sleep(deployDelay)
		}

		if _, s.contractReference, tx, err = swap.Deploy(opts, backend, owner, defaultHarddepositTimeoutDuration); err != nil {
			log.Warn("can't send chequebook deploy tx", "try", try, "error", err)
			continue
		}
		if addr, err = bind.WaitDeployed(opts.Context, backend, tx); err != nil {
			log.Warn("chequebook deploy error", "try", try, "error", err)
			continue
		}
		return addr, nil
	}
	return addr, err
}