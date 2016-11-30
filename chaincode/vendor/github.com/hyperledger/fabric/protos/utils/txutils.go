/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package utils

import (
	"errors"
	"fmt"

	"bytes"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/core/crypto/primitives"
	"github.com/hyperledger/fabric/msp"
	"github.com/hyperledger/fabric/protos/common"
	"github.com/hyperledger/fabric/protos/peer"
)

// GetPayloads get's the underlying payload objects in a TransactionAction
func GetPayloads(txActions *peer.TransactionAction) (*peer.ChaincodeActionPayload, *peer.ChaincodeAction, error) {
	// TODO: pass in the tx type (in what follows we're assuming the type is ENDORSER_TRANSACTION)
	ccPayload := &peer.ChaincodeActionPayload{}
	err := proto.Unmarshal(txActions.Payload, ccPayload)
	if err != nil {
		return nil, nil, err
	}

	if ccPayload.Action == nil || ccPayload.Action.ProposalResponsePayload == nil {
		return nil, nil, fmt.Errorf("no payload in ChaincodeActionPayload")
	}
	pRespPayload := &peer.ProposalResponsePayload{}
	err = proto.Unmarshal(ccPayload.Action.ProposalResponsePayload, pRespPayload)
	if err != nil {
		return nil, nil, err
	}

	if pRespPayload.Extension == nil {
		return nil, nil, err
	}

	respPayload := &peer.ChaincodeAction{}
	err = proto.Unmarshal(pRespPayload.Extension, respPayload)
	if err != nil {
		return ccPayload, nil, err
	}
	return ccPayload, respPayload, nil
}

// GetEndorserTxFromBlock gets Transaction2 from Block.Data.Data
func GetEnvelopeFromBlock(data []byte) (*common.Envelope, error) {
	//Block always begins with an envelope
	var err error
	env := &common.Envelope{}
	if err = proto.Unmarshal(data, env); err != nil {
		return nil, fmt.Errorf("Error getting envelope(%s)\n", err)
	}

	return env, nil
}

// CreateSignedTx assembles an Envelope message from proposal, endorsements and a signer.
// This function should be called by a client when it has collected enough endorsements
// for a proposal to create a transaction and submit it to peers for ordering
func CreateSignedTx(proposal *peer.Proposal, signer msp.SigningIdentity, resps ...*peer.ProposalResponse) (*common.Envelope, error) {
	// the original header
	hdr, err := GetHeader(proposal.Header)
	if err != nil {
		return nil, fmt.Errorf("Could not unmarshal the proposal header")
	}
	// check that the signer is the same that is referenced in the header
	// TODO: maybe worth removing?
	signerBytes, err := signer.Serialize()
	if err != nil {
		return nil, err
	}

	if bytes.Compare(signerBytes, hdr.SignatureHeader.Creator) != 0 {
		return nil, fmt.Errorf("The signer needs to be the same as the one referenced in the header")
	}

	// create the payload
	txEnvelope, err := ConstructUnsignedTxEnvelope(proposal, resps...)
	if err != nil {
		return nil, err
	}
	// sign the payload
	sig, err := signer.Sign(txEnvelope.Payload)
	if err != nil {
		return nil, err
	}
	txEnvelope.Signature = sig
	return txEnvelope, nil
}

// ConstructUnsignedTxEnvelope constructs payload for the transaction from proposal and endorsements.
func ConstructUnsignedTxEnvelope(proposal *peer.Proposal, resps ...*peer.ProposalResponse) (*common.Envelope, error) {
	if len(resps) == 0 {
		return nil, fmt.Errorf("At least one proposal response is necessary")
	}

	// the original header
	hdr, err := GetHeader(proposal.Header)
	if err != nil {
		return nil, fmt.Errorf("Could not unmarshal the proposal header")
	}

	// the original payload
	pPayl, err := GetChaincodeProposalPayload(proposal.Payload)
	if err != nil {
		return nil, fmt.Errorf("Could not unmarshal the proposal payload")
	}

	// get header extensions so we have the visibility field
	hdrExt, err := GetChaincodeHeaderExtension(hdr)
	if err != nil {
		return nil, err
	}

	// ensure that all actions are bitwise equal and that they are successful
	var a1 []byte
	for n, r := range resps {
		if n == 0 {
			a1 = r.Payload
			if r.Response.Status != 200 {
				return nil, fmt.Errorf("Proposal response was not successful, error code %d, msg %s", r.Response.Status, r.Response.Message)
			}
			continue
		}

		if bytes.Compare(a1, r.Payload) != 0 {
			return nil, fmt.Errorf("ProposalResponsePayloads do not match")
		}
	}

	// fill endorsements
	endorsements := make([]*peer.Endorsement, len(resps))
	for n, r := range resps {
		endorsements[n] = r.Endorsement
	}

	// create ChaincodeEndorsedAction
	cea := &peer.ChaincodeEndorsedAction{ProposalResponsePayload: resps[0].Payload, Endorsements: endorsements}

	// obtain the bytes of the proposal payload that will go to the transaction
	propPayloadBytes, err := GetBytesProposalPayloadForTx(pPayl, hdrExt.PayloadVisibility)
	if err != nil {
		return nil, err
	}

	// get the bytes of the signature header, that will be the header of the TransactionAction
	sHdrBytes, err := GetBytesSignatureHeader(hdr.SignatureHeader)
	if err != nil {
		return nil, err
	}

	// serialize the chaincode action payload
	cap := &peer.ChaincodeActionPayload{ChaincodeProposalPayload: propPayloadBytes, Action: cea}
	capBytes, err := GetBytesChaincodeActionPayload(cap)
	if err != nil {
		return nil, err
	}

	// create a transaction
	taa := &peer.TransactionAction{Header: sHdrBytes, Payload: capBytes}
	taas := make([]*peer.TransactionAction, 1)
	taas[0] = taa
	tx := &peer.Transaction{Actions: taas}

	// serialize the tx
	txBytes, err := GetBytesTransaction(tx)
	if err != nil {
		return nil, err
	}
	payl := &common.Payload{Header: hdr, Data: txBytes}
	paylBytes, err := GetBytesPayload(payl)
	if err != nil {
		return nil, err
	}

	// here's the envelope
	return &common.Envelope{Payload: paylBytes, Signature: nil}, nil
}

// CreateProposalResponse creates the proposal response and endorses the payload
func CreateProposalResponse(hdr []byte, payl []byte, results []byte, events []byte, visibility []byte, signingEndorser msp.SigningIdentity) (*peer.ProposalResponse, error) {
	resp, err := ConstructUnsignedProposalResponse(hdr, payl, results, events, visibility)
	if err != nil {
		return nil, err
	}
	// serialize the signing identity
	endorser, err := signingEndorser.Serialize()
	if err != nil {
		return nil, fmt.Errorf("Could not serialize the signing identity for %s, err %s", signingEndorser.Identifier(), err)
	}

	// sign the concatenation of the proposal response and the serialized endorser identity with this endorser's key
	signature, err := signingEndorser.Sign(append(resp.Payload, endorser...))
	if err != nil {
		return nil, fmt.Errorf("Could not sign the proposal response payload, err %s", err)
	}
	resp.Endorsement.Endorser = endorser
	resp.Endorsement.Signature = signature
	return resp, nil
}

// ConstructUnsignedProposalResponse constructs the proposal response structure only
func ConstructUnsignedProposalResponse(hdr []byte, payl []byte, results []byte, events []byte, visibility []byte) (*peer.ProposalResponse, error) {
	// obtain the proposal hash given proposal header, payload and the requested visibility
	pHashBytes, err := GetProposalHash1(hdr, payl, visibility)
	if err != nil {
		return nil, fmt.Errorf("Could not compute proposal hash: err %s", err)
	}

	// get the bytes of the proposal response payload - we need to sign them
	prpBytes, err := GetBytesProposalResponsePayload(pHashBytes, results, events)
	if err != nil {
		return nil, errors.New("Failure while unmarshalling the ProposalResponsePayload")
	}
	resp := &peer.ProposalResponse{
		// Timestamp: TODO!
		Version:     1, // TODO: pick right version number
		Endorsement: &peer.Endorsement{Signature: nil, Endorser: nil},
		Payload:     prpBytes,
		Response:    &peer.Response{Status: 200, Message: "OK"}}

	return resp, nil
}

// GetSignedProposal returns a signed proposal given a Proposal message and a signing identity
func GetSignedProposal(prop *peer.Proposal, signer msp.SigningIdentity) (*peer.SignedProposal, error) {
	// check for nil argument
	if prop == nil || signer == nil {
		return nil, fmt.Errorf("Nil arguments")
	}

	propBytes, err := GetBytesProposal(prop)
	if err != nil {
		return nil, err
	}

	signature, err := signer.Sign(propBytes)
	if err != nil {
		return nil, err
	}

	return &peer.SignedProposal{ProposalBytes: propBytes, Signature: signature}, nil
}

// GetBytesProposalPayloadForTx takes a ChaincodeProposalPayload and returns its serialized
// version according to the visibility field
func GetBytesProposalPayloadForTx(payload *peer.ChaincodeProposalPayload, visibility []byte) ([]byte, error) {
	// check for nil argument
	if payload == nil /* || visibility == nil */ {
		return nil, fmt.Errorf("Nil arguments")
	}

	// strip the transient bytes off the payload - this needs to be done no matter the visibility mode
	cppNoTransient := &peer.ChaincodeProposalPayload{Input: payload.Input, Transient: nil}
	cppBytes, err := GetBytesChaincodeProposalPayload(cppNoTransient)
	if err != nil {
		return nil, errors.New("Failure while marshalling the ChaincodeProposalPayload!")
	}

	// TODO: handle payload visibility - it needs to be defined first!
	// here, as an example, I'll code the visibility policy that allows the
	// full header but only the hash of the payload

	// TODO: use bccsp interfaces and providers as soon as they are ready!
	hash := primitives.GetDefaultHash()()
	hash.Write(cppBytes) // hash the serialized ChaincodeProposalPayload object (stripped of the transient bytes)

	return hash.Sum(nil), nil
}

// GetProposalHash2 gets the proposal hash - this version
// is called by the committer where the visibility policy
// has already been enforced and so we already get what
// we have to get in ccPropPayl
func GetProposalHash2(header []byte, ccPropPayl []byte) ([]byte, error) {
	// check for nil argument
	if header == nil || ccPropPayl == nil {
		return nil, fmt.Errorf("Nil arguments")
	}

	// TODO: use bccsp interfaces and providers as soon as they are ready!
	hash := primitives.GetDefaultHash()()
	hash.Write(header)     // hash the serialized Header object
	hash.Write(ccPropPayl) // hash the bytes of the chaincode proposal payload that we are given

	return hash.Sum(nil), nil
}

// GetProposalHash1 gets the proposal hash bytes after sanitizing the
// chaincode proposal payload according to the rules of visibility
func GetProposalHash1(header []byte, ccPropPayl []byte, visibility []byte) ([]byte, error) {
	// check for nil argument
	if header == nil || ccPropPayl == nil /* || visibility == nil */ {
		return nil, fmt.Errorf("Nil arguments")
	}

	// unmarshal the chaincode proposal payload
	cpp := &peer.ChaincodeProposalPayload{}
	err := proto.Unmarshal(ccPropPayl, cpp)
	if err != nil {
		return nil, errors.New("Failure while unmarshalling the ChaincodeProposalPayload!")
	}

	ppBytes, err := GetBytesProposalPayloadForTx(cpp, visibility)
	if err != nil {
		return nil, err
	}

	// TODO: use bccsp interfaces and providers as soon as they are ready!
	hash2 := primitives.GetDefaultHash()()
	hash2.Write(header)  // hash the serialized Header object
	hash2.Write(ppBytes) // hash of the part of the chaincode proposal payload that will go to the tx

	return hash2.Sum(nil), nil
}