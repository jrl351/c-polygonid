package c_polygonid

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"runtime/trace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	onchainABI "github.com/iden3/contracts-abi/onchain-credential-status-resolver/go/abi"
	"github.com/iden3/contracts-abi/state/go/abi"
	"github.com/iden3/go-circuits/v2"
	core "github.com/iden3/go-iden3-core/v2"
	"github.com/iden3/go-iden3-core/v2/w3c"
	"github.com/iden3/go-iden3-crypto/babyjub"
	"github.com/iden3/go-iden3-crypto/poseidon"
	"github.com/iden3/go-iden3-crypto/utils"
	"github.com/iden3/go-merkletree-sql/v2"
	json2 "github.com/iden3/go-schema-processor/v2/json"
	"github.com/iden3/go-schema-processor/v2/loaders"
	"github.com/iden3/go-schema-processor/v2/merklize"
	"github.com/iden3/go-schema-processor/v2/processor"
	"github.com/iden3/go-schema-processor/v2/verifiable"
	mp "github.com/iden3/merkletree-proof"
	mpHttp "github.com/iden3/merkletree-proof/http"
	"github.com/piprate/json-gold/ld"
)

const mtLevels = 40

type jsonObj = map[string]any

//go:embed schemas/credentials-v1.json-ld
var credentialsV1JsonLDBytes []byte

func stringByPath(obj jsonObj, path string) (string, error) {
	v, err := getByPath(obj, path)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("not a string at path: %v", path)
	}
	return s, nil
}

func bigIntOrZeroByPath(obj jsonObj, path string,
	allowNumbers bool) (*big.Int, error) {

	i, err := bigIntByPath(obj, path, allowNumbers)
	if errors.As(err, &errPathNotFound{}) {
		return big.NewInt(0), nil
	}
	return i, err
}

// if allowNumbers is true, then the value can also be a number, not only strings
func bigIntByPath(obj jsonObj, path string,
	allowNumbers bool) (*big.Int, error) {

	v, err := getByPath(obj, path)
	if err != nil {
		return nil, err
	}

	switch vt := v.(type) {
	case string:
		i, ok := new(big.Int).SetString(vt, 10)
		if !ok {
			return nil, errors.New("not a big int")
		}
		return i, nil
	case float64:
		if !allowNumbers {
			return nil, errors.New("not a string")
		}
		ii := int64(vt)
		if float64(ii) != vt {
			return nil, errors.New("not an int")
		}
		return big.NewInt(0).SetInt64(ii), nil
	default:
		return nil, errors.New("not a string")
	}
}

func objByBath(proof jsonObj, s string) (jsonObj, error) {
	v, err := getByPath(proof, s)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(jsonObj)
	if !ok {
		return nil, errors.New("not an object")
	}
	return obj, nil
}

type errPathNotFound struct {
	path string
}

func (e errPathNotFound) Error() string {
	return fmt.Sprintf("path not found: %v", e.path)
}

func getByPath(obj jsonObj, path string) (any, error) {
	parts := strings.Split(path, ".")

	var curObj = obj
	for i, part := range parts {
		if part == "" {
			return nil, errors.New("path is empty")
		}
		if i == len(parts)-1 {
			v, ok := curObj[part]
			if !ok {
				return nil, errPathNotFound{path}
			}
			return v, nil
		}

		nextObj, ok := curObj[part]
		if !ok {
			return nil, errPathNotFound{path}
		}
		curObj, ok = nextObj.(jsonObj)
		if !ok {
			return nil, errors.New("not a json object")
		}
	}

	return nil, errors.New("should not happen")
}

func resolveRevocationStatusFromIssuerService(ctx context.Context,
	url string) (out circuits.MTProof, err error) {

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url,
		http.NoBody)
	if err != nil {
		return out, err
	}
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer func() {
		err2 := httpResp.Body.Close()
		if err == nil {
			err = err2
		}
	}()
	if httpResp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("unexpected status code: %v",
			httpResp.StatusCode)
	}
	respData, err := io.ReadAll(io.LimitReader(httpResp.Body, 16*1024))
	if err != nil {
		return out, err
	}
	var obj struct {
		TreeState struct {
			State          *hexHash `json:"state"`              // identity state
			ClaimsRoot     *hexHash `json:"claimsTreeRoot"`     // claims tree root
			RevocationRoot *hexHash `json:"revocationTreeRoot"` // revocation tree root
			RootOfRoots    *hexHash `json:"rootOfRoots"`        // root of roots tree root

		} `json:"issuer"`
		Proof *merkletree.Proof `json:"mtp"`
	}
	err = json.Unmarshal(respData, &obj)
	if err != nil {
		return out, err
	}
	out.Proof = obj.Proof
	out.TreeState.State = (*merkletree.Hash)(obj.TreeState.State)
	out.TreeState.ClaimsRoot = (*merkletree.Hash)(obj.TreeState.ClaimsRoot)
	out.TreeState.RevocationRoot = (*merkletree.Hash)(obj.TreeState.RevocationRoot)
	if out.TreeState.RevocationRoot == nil {
		out.TreeState.RevocationRoot = &merkletree.Hash{}
	}
	out.TreeState.RootOfRoots = (*merkletree.Hash)(obj.TreeState.RootOfRoots)
	if out.TreeState.RootOfRoots == nil {
		out.TreeState.RootOfRoots = &merkletree.Hash{}
	}
	return out, nil
}

type errProofNotFound verifiable.ProofType

func (e errProofNotFound) Error() string {
	return fmt.Sprintf("proof not found: %v", string(e))
}

func claimWithSigProofFromObj(ctx context.Context, cfg EnvConfig,
	w3cCred verifiable.W3CCredential,
	skipClaimRevocationCheck bool) (circuits.ClaimWithSigProof, error) {

	var out circuits.ClaimWithSigProof

	proofI := findProofByType(w3cCred, verifiable.BJJSignatureProofType)
	if proofI == nil {
		return out, errProofNotFound(verifiable.BJJSignatureProofType)
	}

	var err error
	proof, ok := proofI.(*verifiable.BJJSignatureProof2021)
	if !ok {
		return out, errors.New("proof is not of type BJJSignatureProof2021")
	}
	issuerDID, err := w3c.ParseDID(proof.IssuerData.ID)
	if err != nil {
		return out, err
	}
	issuerID, err := core.IDFromDID(*issuerDID)
	if err != nil {
		return out, err
	}
	out.IssuerID = &issuerID
	out.Claim, err = proof.GetCoreClaim()
	if err != nil {
		return out, err
	}

	credStatus, ok := w3cCred.CredentialStatus.(jsonObj)
	if !ok {
		return out, errors.New("not a json object")
	}
	out.NonRevProof, err = buildAndValidateCredentialStatus(ctx, cfg,
		credStatus, out.IssuerID, skipClaimRevocationCheck)
	if err != nil {
		return out, err
	}
	out.SignatureProof, err = signatureProof(ctx, cfg, *proof, out.IssuerID)
	if err != nil {
		return out, err
	}

	return out, nil
}

func buildAndValidateCredentialStatus(ctx context.Context, cfg EnvConfig,
	credStatus jsonObj, issuerID *core.ID,
	skipClaimRevocationCheck bool) (circuits.MTProof, error) {

	proof, err := resolveRevStatus(ctx, cfg, credStatus, issuerID)
	if err != nil {
		return proof, err
	}

	if skipClaimRevocationCheck {
		return proof, nil
	}

	treeStateOk, err := validateTreeState(proof.TreeState)
	if err != nil {
		return proof, err
	}
	if !treeStateOk {
		return proof, errors.New("invalid tree state")
	}

	// revocationNonce is float64, but if we meet valid string representation
	// of Int, we will use it.
	// circuits.MTProof
	revNonce, err := bigIntByPath(credStatus, "revocationNonce", true)
	if err != nil {
		return proof, err
	}

	proofValid := merkletree.VerifyProof(proof.TreeState.RevocationRoot,
		proof.Proof, revNonce, big.NewInt(0))
	if !proofValid {
		return proof, fmt.Errorf("proof validation failed. revNonce=%d", revNonce)
	}

	if proof.Proof.Existence {
		return proof, errors.New("credential is revoked")
	}

	return proof, nil
}

// check TreeState consistency
func validateTreeState(s circuits.TreeState) (bool, error) {
	if s.State == nil {
		return false, errors.New("state is nil")
	}

	ctrHash := &merkletree.HashZero
	if s.ClaimsRoot != nil {
		ctrHash = s.ClaimsRoot
	}
	rtrHash := &merkletree.HashZero
	if s.RevocationRoot != nil {
		rtrHash = s.RevocationRoot
	}
	rorHash := &merkletree.HashZero
	if s.RootOfRoots != nil {
		rorHash = s.RootOfRoots
	}

	wantState, err := poseidon.Hash([]*big.Int{ctrHash.BigInt(),
		rtrHash.BigInt(), rorHash.BigInt()})
	if err != nil {
		return false, err
	}

	return wantState.Cmp(s.State.BigInt()) == 0, nil
}

func sigFromHex(sigHex string) (*babyjub.Signature, error) {
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return nil, err
	}
	var compSig babyjub.SignatureComp
	if len(sigBytes) != len(compSig) {
		return nil, fmt.Errorf("signature length is not %v", len(compSig))
	}
	copy(compSig[:], sigBytes)
	return compSig.Decompress()
}

func signatureProof(ctx context.Context, cfg EnvConfig,
	proof verifiable.BJJSignatureProof2021,
	issuerID *core.ID) (out circuits.BJJSignatureProof, err error) {

	out.Signature, err = sigFromHex(proof.Signature)
	if err != nil {
		return out, err
	}
	out.IssuerAuthClaim = new(core.Claim)
	err = out.IssuerAuthClaim.FromHex(proof.IssuerData.AuthCoreClaim)
	if err != nil {
		return
	}
	out.IssuerAuthIncProof.TreeState, err = circuitsTreeStateFromSchemaState(proof.IssuerData.State)
	if err != nil {
		return out, err
	}
	out.IssuerAuthIncProof.Proof = proof.IssuerData.MTP
	credStatus, ok := proof.IssuerData.CredentialStatus.(jsonObj)
	if !ok {
		return out, errors.New("credential status is not of object type")
	}
	out.IssuerAuthNonRevProof, err =
		buildAndValidateCredentialStatus(ctx, cfg, credStatus, issuerID, false)
	if err != nil {
		return out, err
	}

	return out, nil
}

func findProofByType(w3cCred verifiable.W3CCredential,
	proofType verifiable.ProofType) verifiable.CredentialProof {

	for _, p := range w3cCred.Proof {
		if p.ProofType() == proofType {
			return p
		}
	}

	return nil
}

type inputsRequest struct {
	ID                       core.ID         `json:"id"`
	ProfileNonce             JsonBigInt      `json:"profileNonce"`
	ClaimSubjectProfileNonce JsonBigInt      `json:"claimSubjectProfileNonce"`
	VerifiableCredentials    json.RawMessage `json:"verifiableCredentials"`
	Request                  jsonObj         `json:"request"`
}

type v3InputsRequest struct {
	inputsRequest
	VerifierID *w3c.DID   `json:"verifierId"`
	LinkNonce  JsonBigInt `json:"linkNonce"`
}

type onChainInputsRequest struct {
	ID                       *core.ID            `json:"id"`
	ProfileNonce             *JsonBigInt         `json:"profileNonce"`
	ClaimSubjectProfileNonce *JsonBigInt         `json:"claimSubjectProfileNonce"`
	AuthClaim                *core.Claim         `json:"authClaim"`
	AuthClaimIncMtp          *merkletree.Proof   `json:"authClaimIncMtp"`
	AuthClaimNonRevMtp       *merkletree.Proof   `json:"authClaimNonRevMtp"`
	TreeState                *circuits.TreeState `json:"treeState"`
	GistProof                *circuits.GISTProof `json:"gistProof"`
	Signature                *hexSigJson         `json:"signature"`
	Challenge                *JsonBigInt         `json:"challenge"`
	VerifiableCredentials    json.RawMessage     `json:"verifiableCredentials"`
	Request                  jsonObj             `json:"request"`
}

type txData struct {
	ContractAddress common.Address `json:"contractAddress"`
	ChainID         ChainID        `json:"chainId"`
}

type v3OnChainInputsRequest struct {
	onChainInputsRequest
	VerifierID *w3c.DID   `json:"verifierId"`
	LinkNonce  JsonBigInt `json:"linkNonce"`
	TxData     *txData    `json:"transactionData"`
}

type AtomicQueryInputsResponse struct {
	Inputs                 circuits.InputsMarshaller
	VerifiablePresentation map[string]any
}

func AtomicQueryMtpV2InputsFromJson(ctx context.Context, cfg EnvConfig,
	in []byte) (AtomicQueryInputsResponse, error) {

	ctx, task := trace.NewTask(ctx, "AtomicQueryMtpV2InputsFromJson")
	defer task.End()

	var out AtomicQueryInputsResponse
	var inpMarsh circuits.AtomicQueryMTPV2Inputs

	var obj inputsRequest
	err := json.Unmarshal(in, &obj)
	if err != nil {
		return out, err
	}

	inpMarsh.RequestID, err = bigIntByPath(obj.Request, "id", true)
	if err != nil {
		return out, err
	}
	inpMarsh.ID = &obj.ID
	inpMarsh.ProfileNonce = obj.ProfileNonce.BigInt()
	inpMarsh.ClaimSubjectProfileNonce = obj.ClaimSubjectProfileNonce.BigInt()

	circuitID, err := getCircuitID(obj.Request)
	if err != nil {
		return out, err
	}
	if circuitID != circuits.AtomicQueryMTPV2CircuitID {
		return out, errors.New("wrong circuit")
	}

	var w3cCred verifiable.W3CCredential
	err = json.Unmarshal(obj.VerifiableCredentials, &w3cCred)
	if err != nil {
		return out, err
	}

	inpMarsh.SkipClaimRevocationCheck, err = querySkipRevocation(obj.Request)
	if err != nil {
		return out, err
	}

	var wg sync.WaitGroup

	var queryErr error
	var proofErr error

	onClaimReady := func(claim *core.Claim) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			inpMarsh.Query, out.VerifiablePresentation, queryErr = queryFromObj(
				ctx, w3cCred, obj.Request, claim, cfg.documentLoader(),
				circuitID)
			slog.Debug("query done in", "time", time.Since(start))
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		inpMarsh.Claim, proofErr = claimWithMtpProofFromObj(ctx, cfg, w3cCred,
			inpMarsh.SkipClaimRevocationCheck, onClaimReady)
		slog.Debug("rev proof done in", "time", time.Since(start))
	}()

	wg.Wait()
	if proofErr != nil {
		return out, proofErr
	}
	if queryErr != nil {
		return out, queryErr
	}

	inpMarsh.CurrentTimeStamp = time.Now().Unix()

	out.Inputs = inpMarsh

	return out, nil
}

func verifiablePresentationFromCred(ctx context.Context,
	w3cCred verifiable.W3CCredential, requestObj jsonObj, field string,
	documentLoader ld.DocumentLoader) (verifiablePresentation map[string]any,
	mzValue merklize.Value, datatype string, hasher merklize.Hasher,
	err error) {

	var mz *merklize.Merklizer
	mz, err = wrapMerklizeWithRegion(ctx, w3cCred, documentLoader)
	if err != nil {
		return nil, nil, datatype, hasher, err
	}

	hasher = mz.Hasher()

	var contextType string
	contextType, err = stringByPath(requestObj, "query.type")
	if err != nil {
		return nil, nil, datatype, hasher, err
	}

	var contextURL string
	contextURL, err = stringByPath(requestObj, "query.context")
	if err != nil {
		return nil, nil, datatype, hasher, err
	}

	path, err := buildQueryPath(ctx, contextURL, contextType, field,
		documentLoader)
	if err != nil {
		return nil, nil, datatype, hasher, err
	}

	datatype, err = mz.JSONLDType(path)
	if err != nil {
		return nil, nil, datatype, hasher, err
	}

	rawValue, err := mz.RawValue(path)
	if err != nil {
		return nil, nil, datatype, hasher, err
	}

	_, mzValue, err = mz.Proof(ctx, path)
	if err != nil {
		return nil, nil, datatype, hasher, err
	}

	verifiablePresentation = fmtVerifiablePresentation(contextURL,
		contextType, field, rawValue)

	return
}

func mkVPObj(field string, value any) (string, any) {
	idx := strings.Index(field, ".")
	if idx == -1 {
		return field, value
	}

	nestedField, value := mkVPObj(field[idx+1:], value)
	return field[:idx], map[string]any{nestedField: value}
}

func fmtVerifiablePresentation(context string, tp string, field string,
	value any) map[string]any {

	var ldContext any

	var baseContext = []any{"https://www.w3.org/2018/credentials/v1"}
	if context == baseContext[0] {
		ldContext = baseContext
	} else {
		ldContext = append(baseContext, context)
	}

	vcTypes := []any{"VerifiableCredential"}
	if tp != "VerifiableCredential" {
		vcTypes = append(vcTypes, tp)
	}

	// if field name is a dot-separated path, create nested object from it.
	field, value = mkVPObj(field, value)

	return map[string]any{
		"@context": baseContext,
		"@type":    "VerifiablePresentation",
		"verifiableCredential": map[string]any{
			"@context": ldContext,
			"@type":    vcTypes,
			"credentialSubject": map[string]any{
				"@type": tp,
				field:   value,
			},
		},
	}
}

func AtomicQuerySigV2InputsFromJson(ctx context.Context, cfg EnvConfig,
	in []byte) (AtomicQueryInputsResponse, error) {

	var out AtomicQueryInputsResponse
	var inpMarsh circuits.AtomicQuerySigV2Inputs

	var obj inputsRequest
	err := json.Unmarshal(in, &obj)
	if err != nil {
		return out, err
	}

	inpMarsh.RequestID, err = bigIntByPath(obj.Request, "id", true)
	if err != nil {
		return out, err
	}
	inpMarsh.ID = &obj.ID
	inpMarsh.ProfileNonce = obj.ProfileNonce.BigInt()
	inpMarsh.ClaimSubjectProfileNonce = obj.ClaimSubjectProfileNonce.BigInt()

	circuitID, err := getCircuitID(obj.Request)
	if err != nil {
		return out, err
	}
	if circuitID != circuits.AtomicQuerySigV2CircuitID {
		return out, errors.New("wrong circuit")
	}

	var w3cCred verifiable.W3CCredential
	err = json.Unmarshal(obj.VerifiableCredentials, &w3cCred)
	if err != nil {
		return out, err
	}

	inpMarsh.SkipClaimRevocationCheck, err = querySkipRevocation(obj.Request)
	if err != nil {
		return out, err
	}
	inpMarsh.Claim, err = claimWithSigProofFromObj(ctx, cfg, w3cCred,
		inpMarsh.SkipClaimRevocationCheck)
	if err != nil {
		return out, err
	}

	inpMarsh.Query, out.VerifiablePresentation, err = queryFromObj(ctx, w3cCred,
		obj.Request, inpMarsh.Claim.Claim, cfg.documentLoader(), circuitID)
	if err != nil {
		return out, err
	}

	inpMarsh.CurrentTimeStamp = time.Now().Unix()

	out.Inputs = inpMarsh

	return out, nil
}

func AtomicQueryMtpV2OnChainInputsFromJson(ctx context.Context, cfg EnvConfig,
	in []byte) (AtomicQueryInputsResponse, error) {

	var out AtomicQueryInputsResponse
	var inpMarsh circuits.AtomicQueryMTPV2OnChainInputs

	var obj onChainInputsRequest
	err := json.Unmarshal(in, &obj)
	if err != nil {
		return out, err
	}

	inpMarsh.RequestID, err = bigIntByPath(obj.Request, "id", true)
	if err != nil {
		return out, err
	}

	if obj.ID == nil {
		return out, errors.New(`"id" field is required`)
	}

	inpMarsh.ID = obj.ID
	inpMarsh.ProfileNonce = obj.ProfileNonce.BigInt()
	inpMarsh.ClaimSubjectProfileNonce = obj.ClaimSubjectProfileNonce.BigInt()

	inpMarsh.AuthClaim = obj.AuthClaim
	inpMarsh.AuthClaimIncMtp = obj.AuthClaimIncMtp
	inpMarsh.AuthClaimNonRevMtp = obj.AuthClaimNonRevMtp

	if obj.TreeState == nil {
		return out, errors.New("treeState is required")
	}
	inpMarsh.TreeState = *obj.TreeState

	if obj.GistProof == nil {
		return out, errors.New("gistProof is required")
	}
	inpMarsh.GISTProof = *obj.GistProof

	inpMarsh.Signature = (*babyjub.Signature)(obj.Signature)
	inpMarsh.Challenge = obj.Challenge.BigInt()

	circuitID, err := getCircuitID(obj.Request)
	if err != nil {
		return out, err
	}
	if circuitID != circuits.AtomicQueryMTPV2OnChainCircuitID {
		return out, errors.New("wrong circuit")
	}

	var w3cCred verifiable.W3CCredential
	err = json.Unmarshal(obj.VerifiableCredentials, &w3cCred)
	if err != nil {
		return out, err
	}

	inpMarsh.SkipClaimRevocationCheck, err = querySkipRevocation(obj.Request)
	if err != nil {
		return out, err
	}

	var wg sync.WaitGroup

	var queryErr error
	var proofErr error

	onClaimReady := func(claim *core.Claim) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inpMarsh.Query, out.VerifiablePresentation, queryErr = queryFromObj(
				ctx, w3cCred, obj.Request, claim, cfg.documentLoader(),
				circuitID)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		inpMarsh.Claim, proofErr = claimWithMtpProofFromObj(ctx, cfg, w3cCred,
			inpMarsh.SkipClaimRevocationCheck, onClaimReady)
	}()

	wg.Wait()
	if proofErr != nil {
		return out, proofErr
	}
	if queryErr != nil {
		return out, queryErr
	}

	inpMarsh.CurrentTimeStamp = time.Now().Unix()

	out.Inputs = inpMarsh

	return out, nil
}

func AtomicQuerySigV2OnChainInputsFromJson(ctx context.Context, cfg EnvConfig,
	in []byte) (AtomicQueryInputsResponse, error) {

	var out AtomicQueryInputsResponse
	var inpMarsh circuits.AtomicQuerySigV2OnChainInputs

	var obj onChainInputsRequest
	err := json.Unmarshal(in, &obj)
	if err != nil {
		return out, err
	}

	inpMarsh.RequestID, err = bigIntByPath(obj.Request, "id", true)
	if err != nil {
		return out, err
	}

	if obj.ID == nil {
		return out, errors.New(`"id" field is required`)
	}

	inpMarsh.ID = obj.ID
	inpMarsh.ProfileNonce = obj.ProfileNonce.BigInt()
	inpMarsh.ClaimSubjectProfileNonce = obj.ClaimSubjectProfileNonce.BigInt()

	inpMarsh.AuthClaim = obj.AuthClaim
	inpMarsh.AuthClaimIncMtp = obj.AuthClaimIncMtp
	inpMarsh.AuthClaimNonRevMtp = obj.AuthClaimNonRevMtp

	if obj.TreeState == nil {
		return out, errors.New("treeState is required")
	}
	inpMarsh.TreeState = *obj.TreeState

	if obj.GistProof == nil {
		return out, errors.New("gistProof is required")
	}
	inpMarsh.GISTProof = *obj.GistProof

	inpMarsh.Signature = (*babyjub.Signature)(obj.Signature)
	inpMarsh.Challenge = obj.Challenge.BigInt()

	circuitID, err := getCircuitID(obj.Request)
	if err != nil {
		return out, err
	}
	if circuitID != circuits.AtomicQuerySigV2OnChainCircuitID {
		return out, errors.New("wrong circuit")
	}

	var w3cCred verifiable.W3CCredential
	err = json.Unmarshal(obj.VerifiableCredentials, &w3cCred)
	if err != nil {
		return out, err
	}

	inpMarsh.SkipClaimRevocationCheck, err = querySkipRevocation(obj.Request)
	if err != nil {
		return out, err
	}
	inpMarsh.Claim, err = claimWithSigProofFromObj(ctx, cfg, w3cCred,
		inpMarsh.SkipClaimRevocationCheck)
	if err != nil {
		return out, err
	}

	inpMarsh.Query, out.VerifiablePresentation, err = queryFromObj(ctx, w3cCred,
		obj.Request, inpMarsh.Claim.Claim, cfg.documentLoader(), circuitID)
	if err != nil {
		return out, err
	}

	inpMarsh.CurrentTimeStamp = time.Now().Unix()

	out.Inputs = inpMarsh

	return out, nil
}

func AtomicQueryV3OnChainInputsFromJson(ctx context.Context, cfg EnvConfig,
	in []byte) (AtomicQueryInputsResponse, error) {

	var out AtomicQueryInputsResponse
	var inpMarsh circuits.AtomicQueryV3OnChainInputs
	inpMarsh.AuthEnabled = 1

	var obj v3OnChainInputsRequest
	err := json.Unmarshal(in, &obj)
	if err != nil {
		return out, err
	}

	inpMarsh.RequestID, err = bigIntByPath(obj.Request, "id", true)
	if err != nil {
		return out, err
	}

	if obj.ID == nil {
		return out, errors.New(`"id" field is required`)
	}

	inpMarsh.ID = obj.ID
	inpMarsh.ProfileNonce = obj.ProfileNonce.BigInt()
	inpMarsh.ClaimSubjectProfileNonce = obj.ClaimSubjectProfileNonce.BigInt()

	inpMarsh.AuthClaim = obj.AuthClaim
	inpMarsh.AuthClaimIncMtp = obj.AuthClaimIncMtp
	inpMarsh.AuthClaimNonRevMtp = obj.AuthClaimNonRevMtp

	if obj.TreeState == nil {
		return out, errors.New("treeState is required")
	}
	inpMarsh.TreeState = *obj.TreeState

	if obj.GistProof == nil {
		return out, errors.New("gistProof is required")
	}
	inpMarsh.GISTProof = *obj.GistProof

	inpMarsh.Signature = (*babyjub.Signature)(obj.Signature)
	inpMarsh.Challenge = obj.Challenge.BigInt()

	circuitID, err := getCircuitID(obj.Request)
	if err != nil {
		return out, err
	}
	if circuitID != circuits.AtomicQueryV3OnChainCircuitID {
		return out, errors.New("wrong circuit")
	}

	var w3cCred verifiable.W3CCredential
	err = json.Unmarshal(obj.VerifiableCredentials, &w3cCred)
	if err != nil {
		return out, err
	}

	inpMarsh.SkipClaimRevocationCheck, err = querySkipRevocation(obj.Request)
	if err != nil {
		return out, err
	}

	reqProofType, err := queryProofType(obj.Request)
	if err != nil {
		return out, err
	}

	inpMarsh.Claim, inpMarsh.ProofType, err = claimWithSigAndMtpProofFromObj(
		ctx, cfg, w3cCred, inpMarsh.SkipClaimRevocationCheck, reqProofType)
	if err != nil {
		return out, err
	}

	inpMarsh.Query, out.VerifiablePresentation, err = queryFromObj(ctx, w3cCred,
		obj.Request, inpMarsh.Claim.Claim, cfg.documentLoader(), circuitID)
	if err != nil {
		return out, err
	}

	inpMarsh.CurrentTimeStamp = time.Now().Unix()

	inpMarsh.LinkNonce = obj.LinkNonce.BigInt()
	if obj.VerifierID != nil {
		id, err := core.IDFromDID(*obj.VerifierID)
		if err != nil {
			return out, err
		}
		inpMarsh.VerifierID = &id
	} else if obj.TxData != nil {
		inpMarsh.VerifierID, err = verifierIDFromTxData(*obj.TxData)
		if err != nil {
			return out, err
		}
	}

	inpMarsh.NullifierSessionID, err = bigIntOrZeroByPath(obj.Request,
		"params.nullifierSessionId", true)
	if err != nil {
		return out, err
	}

	out.Inputs = inpMarsh

	return out, nil
}

func AtomicQueryV3InputsFromJson(ctx context.Context, cfg EnvConfig,
	in []byte) (AtomicQueryInputsResponse, error) {

	var out AtomicQueryInputsResponse
	var inpMarsh circuits.AtomicQueryV3Inputs

	var obj v3InputsRequest
	err := json.Unmarshal(in, &obj)
	if err != nil {
		return out, err
	}

	inpMarsh.RequestID, err = bigIntByPath(obj.Request, "id", true)
	if err != nil {
		return out, err
	}
	inpMarsh.ID = &obj.ID
	inpMarsh.ProfileNonce = obj.ProfileNonce.BigInt()
	inpMarsh.ClaimSubjectProfileNonce = obj.ClaimSubjectProfileNonce.BigInt()

	circuitID, err := getCircuitID(obj.Request)
	if err != nil {
		return out, err
	}
	if circuitID != circuits.AtomicQueryV3CircuitID {
		return out, errors.New("wrong circuit")
	}

	var w3cCred verifiable.W3CCredential
	err = json.Unmarshal(obj.VerifiableCredentials, &w3cCred)
	if err != nil {
		return out, err
	}

	inpMarsh.SkipClaimRevocationCheck, err = querySkipRevocation(obj.Request)
	if err != nil {
		return out, err
	}

	reqProofType, err := queryProofType(obj.Request)
	if err != nil {
		return out, err
	}

	inpMarsh.Claim, inpMarsh.ProofType, err = claimWithSigAndMtpProofFromObj(
		ctx, cfg, w3cCred, inpMarsh.SkipClaimRevocationCheck, reqProofType)
	if err != nil {
		return out, err
	}

	inpMarsh.Query, out.VerifiablePresentation, err = queryFromObj(ctx, w3cCred,
		obj.Request, inpMarsh.Claim.Claim, cfg.documentLoader(), circuitID)
	if err != nil {
		return out, err
	}

	inpMarsh.CurrentTimeStamp = time.Now().Unix()

	inpMarsh.NullifierSessionID, err = bigIntOrZeroByPath(obj.Request,
		"params.nullifierSessionId", true)
	if err != nil {
		return out, err
	}

	if obj.VerifierID != nil {
		id, err := core.IDFromDID(*obj.VerifierID)
		if err != nil {
			return out, err
		}
		inpMarsh.VerifierID = &id
	}

	inpMarsh.LinkNonce = obj.LinkNonce.BigInt()

	out.Inputs = inpMarsh

	return out, nil
}

// return empty circuits.ProofType if not found
func queryProofType(requestObj jsonObj) (circuits.ProofType, error) {
	result, err := getByPath(requestObj, "query.proofType")
	if errors.As(err, &errPathNotFound{}) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	resS, ok := result.(string)
	if !ok {
		return "", errors.New("value of proofType is not string")
	}

	switch circuits.ProofType(resS) {
	case circuits.Iden3SparseMerkleTreeProofType:
		return circuits.Iden3SparseMerkleTreeProofType, nil
	case circuits.BJJSignatureProofType:
		return circuits.BJJSignatureProofType, nil
	}
	return "", fmt.Errorf("unknown proofType: %v", resS)

}

func buildQueryPath(ctx context.Context, contextURL string, contextType string,
	field string,
	documentLoader ld.DocumentLoader) (path merklize.Path, err error) {

	schemaDoc, err := documentLoader.LoadDocument(contextURL)
	if err != nil {
		return merklize.Path{}, err
	}

	schemaBytes, err := json.Marshal(schemaDoc.Document)
	if err != nil {
		return merklize.Path{}, err
	}
	path, err = merklize.NewFieldPathFromContext(schemaBytes, contextType,
		field)
	if err != nil {
		return
	}
	// took from identity-server prepareMerklizedQuery func
	err = path.Prepend("https://www.w3.org/2018/credentials#credentialSubject")
	if err != nil {
		return
	}

	return
}

func querySkipRevocation(requestObj jsonObj) (bool, error) {
	result, err := getByPath(requestObj, "query.skipClaimRevocationCheck")
	if errors.As(err, &errPathNotFound{}) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	resB, ok := result.(bool)
	if !ok {
		return false,
			errors.New("value of skipClaimRevocationCheck is not bool")
	}
	return resB, nil
}

func queryFromObj(ctx context.Context, w3cCred verifiable.W3CCredential,
	requestObj jsonObj, claim *core.Claim, documentLoader ld.DocumentLoader,
	circuitID circuits.CircuitID) (out circuits.Query,
	verifiablePresentation jsonObj, err error) {

	merklizePosition, err := claim.GetMerklizedPosition()
	if err != nil {
		return out, nil, err
	}

	if merklizePosition == core.MerklizedRootPositionNone {
		return queryFromObjNonMerklized(ctx, w3cCred, requestObj,
			documentLoader, circuitID)
	}

	return queryFromObjMerklized(ctx, w3cCred, requestObj, documentLoader,
		circuitID)
}

func wrapMerklizeWithRegion(ctx context.Context,
	w3cCred verifiable.W3CCredential,
	documentLoader ld.DocumentLoader) (*merklize.Merklizer, error) {

	var mz *merklize.Merklizer
	var err error
	trace.WithRegion(ctx, "merklize", func() {
		mz, err = merklizeCred(ctx, w3cCred, documentLoader, true)
	})
	return mz, err
}

func queryFromObjNonMerklized(ctx context.Context,
	w3cCred verifiable.W3CCredential, requestObj jsonObj,
	documentLoader ld.DocumentLoader,
	circuitID circuits.CircuitID) (out circuits.Query,
	verifiablePresentation jsonObj, err error) {

	region := trace.StartRegion(ctx, "queryFromObjNonMerklized")
	defer region.End()

	pr := processor.InitProcessorOptions(&processor.Processor{
		DocumentLoader: documentLoader,
		Parser:         json2.Parser{},
	})

	field, op, err := getQueryFieldAndOperator(requestObj)
	if errors.As(err, &errPathNotFound{}) {
		out.Operator = circuits.NOOP
		out.Values = []*big.Int{}
		return out, nil, nil
	} else if err != nil {
		return out, nil,
			fmt.Errorf("unable to extract field from query: %w", err)
	}

	schemaURL, typeName, err := getQuerySchemaAndType(requestObj)
	if err != nil {
		return out, nil, err
	}

	schema, err := pr.Load(ctx, schemaURL)
	if err != nil {
		return out, nil, err
	}

	out.SlotIndex, err = pr.GetFieldSlotIndex(field, typeName, schema)
	if err != nil {
		return out, nil, err
	}

	var opObj jsonObj
	var ok bool
	opObj, ok = op.(jsonObj)
	if !ok {
		return out, nil, errors.New("operation on field is not a json object")
	}

	vp, mzValue, datatype, hasher, err := verifiablePresentationFromCred(ctx,
		w3cCred, requestObj, field, documentLoader)
	if err != nil {
		return out, nil, err
	}

	opStr, val, err := extractSingleEntry(opObj)
	switch err {
	case errMultipleEntries:
		return out, nil, errors.New("only one operation per field is supported")
	case errNoEntry:
		// handle selective disclosure
		var valueEntry *big.Int
		valueEntry, err = mzValue.MtEntry()
		if err != nil {
			return out, nil, err
		}

		verifiablePresentation = vp
		if circuitID == circuits.AtomicQueryV3CircuitID ||
			circuitID == circuits.AtomicQueryV3OnChainCircuitID {
			out.Operator = circuits.SD
			out.Values = []*big.Int{}
		} else {
			out.Operator = circuits.EQ
			out.Values = []*big.Int{valueEntry}
		}
	default:
		out.Operator, out.Values, err = unpackOperatorWithArgs(opStr, val,
			datatype, hasher)
		if err != nil {
			return out, nil, err
		}
	}

	return out, verifiablePresentation, nil
}

func getQuerySchemaAndType(requestObj jsonObj) (string, string, error) {
	typeName, err := stringByPath(requestObj, "query.type")
	if err != nil {
		return "", "", err
	}
	schemaURL, err := stringByPath(requestObj, "query.context")
	if err != nil {
		return "", "", err
	}
	return schemaURL, typeName, nil
}

func getCircuitID(requestObj jsonObj) (circuits.CircuitID, error) {
	circuitID, err := stringByPath(requestObj, "circuitId")
	if err != nil {
		return "", err
	}
	return circuits.CircuitID(circuitID), nil
}

func queryFromObjMerklized(ctx context.Context,
	w3cCred verifiable.W3CCredential, requestObj jsonObj,
	documentLoader ld.DocumentLoader,
	circuitID circuits.CircuitID) (out circuits.Query,
	verifiablePresentation jsonObj, err error) {

	region := trace.StartRegion(ctx, "queryFromObjMerklized")
	defer region.End()

	mz, err := wrapMerklizeWithRegion(ctx, w3cCred, documentLoader)
	if err != nil {
		return out, nil, err
	}

	var contextURL string
	contextURL, err = stringByPath(requestObj, "query.context")
	if err != nil {
		return out, nil, err
	}
	var contextType string
	contextType, err = stringByPath(requestObj, "query.type")
	if err != nil {
		return out, nil, err
	}
	field, op, err := getQueryFieldAndOperator(requestObj)
	if errors.As(err, &errPathNotFound{}) {
		out.Operator = circuits.EQ
		var path merklize.Path
		path, err = merklize.NewPath(
			"https://www.w3.org/2018/credentials#credentialSubject")
		if err != nil {
			return out, nil, err
		}
		out.ValueProof = new(circuits.ValueProof)
		var mzValue merklize.Value
		out.ValueProof.MTP, mzValue, err = mz.Proof(ctx, path)
		if err != nil {
			return out, nil, err
		}

		if !out.ValueProof.MTP.Existence {
			return out, nil, fmt.Errorf(
				"value not found in verifiable credential by path %v",
				fmtPath(path))
		}

		var val *big.Int
		val, err = mzValue.MtEntry()
		if err != nil {
			return out, nil, err
		}
		out.Values = []*big.Int{val}
		out.ValueProof.Value = val
		out.ValueProof.Path, err = path.MtEntry()
		if err != nil {
			return out, nil, err
		}
		return out, nil, nil
	} else if err != nil {
		return out, nil,
			fmt.Errorf("unable to extract field from query: %w", err)
	}
	path, err := buildQueryPath(ctx, contextURL, contextType, field,
		documentLoader)
	if err != nil {
		return out, nil, err
	}

	out.ValueProof = new(circuits.ValueProof)
	out.ValueProof.Path, err = path.MtEntry()
	if err != nil {
		return out, nil, err
	}
	var mzValue merklize.Value
	out.ValueProof.MTP, mzValue, err = mz.Proof(ctx, path)
	if err != nil {
		return out, nil, err
	}

	if !out.ValueProof.MTP.Existence {
		return out, nil, fmt.Errorf(
			"value not found in verifiable credential by path %v",
			fmtPath(path))
	}

	out.ValueProof.Value, err = mzValue.MtEntry()
	if err != nil {
		return out, nil, err
	}

	var opObj jsonObj
	var ok bool
	opObj, ok = op.(jsonObj)
	if !ok {
		return out, nil, errors.New("operation on field is not a json object")
	}
	opStr, val, err := extractSingleEntry(opObj)
	switch err {
	case errMultipleEntries:
		return out, nil, errors.New("only one operation per field is supported")
	case errNoEntry:
		// handle selective disclosure
		if circuitID == circuits.AtomicQueryV3CircuitID ||
			circuitID == circuits.AtomicQueryV3OnChainCircuitID {
			out.Operator = circuits.SD
			out.Values = []*big.Int{}
		} else {
			out.Operator = circuits.EQ
			out.Values = []*big.Int{out.ValueProof.Value}
		}

		rawValue, err := mz.RawValue(path)
		if err != nil {
			return out, nil, err
		}
		verifiablePresentation = fmtVerifiablePresentation(contextURL,
			contextType, field, rawValue)
	default:
		fieldDatatype, err := mz.JSONLDType(path)
		if err != nil {
			return out, nil, err
		}

		out.Operator, out.Values, err = unpackOperatorWithArgs(opStr, val,
			fieldDatatype, mz.Hasher())
		if err != nil {
			return out, nil, err
		}
	}
	return out, verifiablePresentation, nil
}

// Return int operator value by its name and arguments as big.Ints array.
func unpackOperatorWithArgs(opStr string, opValue any,
	datatype string, hasher merklize.Hasher) (int, []*big.Int, error) {

	hashFn := func(val any) (*big.Int, error) {
		if hasher == nil {
			return merklize.HashValue(datatype, val)
		} else {
			return merklize.HashValueWithHasher(hasher, datatype, val)
		}
	}

	op, ok := circuits.QueryOperators[opStr]
	if !ok {
		return 0, nil, errors.New("unknown operator")
	}

	var err error
	valArr, isArr := opValue.([]any)
	if isArr {
		vals := make([]*big.Int, len(valArr))
		for i, v := range valArr {
			vals[i], err = hashFn(v)
			if err != nil {
				return 0, nil, err
			}
		}
		return op, vals, nil
	} else {
		vals := make([]*big.Int, 1)
		vals[0], err = hashFn(opValue)
		if err != nil {
			return 0, nil, err
		}
		return op, vals, nil
	}
}

func getQueryFieldAndOperator(requestObj jsonObj) (string, any, error) {
	credSubjObj, err := objByBath(requestObj, "query.credentialSubject")
	if err != nil {
		return "", nil, err
	}
	return extractSingleEntry(credSubjObj)
}

var errNoEntry = errors.New("no entry")
var errMultipleEntries = errors.New("multiple entries")

func extractSingleEntry(obj jsonObj) (key string, val any, err error) {
	if len(obj) > 1 {
		return key, val, errMultipleEntries
	}
	for key, val = range obj {
		return key, val, nil
	}
	return key, val, errNoEntry
}

type hexHash merkletree.Hash

func (h *hexHash) UnmarshalJSON(i []byte) error {
	var str string
	err := json.Unmarshal(i, &str)
	if err != nil {
		return err
	}
	hashBytes, err := hex.DecodeString(str)
	if err != nil {
		return err
	}
	if len(hashBytes) != len(*h) {
		return errors.New("invalid hash length")
	}

	copy(h[:], hashBytes)
	h2 := (*merkletree.Hash)(h)
	bi := h2.BigInt()
	if !utils.CheckBigIntInField(bi) {
		return errors.New("hash is not in the field")
	}
	return nil
}

func claimWithMtpProofFromObj(ctx context.Context, cfg EnvConfig,
	w3cCred verifiable.W3CCredential,
	skipClaimRevocationCheck bool,
	claimProcessFn func(claim *core.Claim)) (circuits.ClaimWithMTPProof, error) {

	region := trace.StartRegion(ctx, "claimWithMtpProofFromObj")
	defer region.End()

	var out circuits.ClaimWithMTPProof
	var err error
	var proofI verifiable.CredentialProof
	var issuerDID *w3c.DID

	if proofI = findProofByType(w3cCred,
		verifiable.Iden3SparseMerkleTreeProofType); proofI != nil {

		proof, ok := proofI.(*verifiable.Iden3SparseMerkleTreeProof)
		if !ok {
			return out, errors.New("proof is not a sparse merkle proof")
		}
		issuerDID, err = w3c.ParseDID(proof.IssuerData.ID)
		if err != nil {
			return out, err
		}
		out.IncProof.Proof = proof.MTP
		out.IncProof.TreeState, err = circuitsTreeStateFromSchemaState(proof.IssuerData.State)
		if err != nil {
			return out, err
		}

	} else if proofI = findProofByType(w3cCred,
		verifiable.Iden3SparseMerkleProofType); proofI != nil { //nolint:staticcheck //reason: need to support deprecated proofs for backward compatibility

		//nolint:staticcheck //reason: need to support deprecated proofs for backward compatibility
		proof, ok := proofI.(*verifiable.Iden3SparseMerkleProof)
		if !ok {
			return out, errors.New("proof is not a sparse merkle proof")
		}
		issuerDID, err = w3c.ParseDID(proof.IssuerData.ID)
		if err != nil {
			return out, err
		}
		out.IncProof.Proof = proof.MTP
		out.IncProof.TreeState, err = circuitsTreeStateFromSchemaState(proof.IssuerData.State)
		if err != nil {
			return out, err
		}

	} else if proofI = findProofByType(w3cCred, verifiable.ProofType(verifiable.Iden3OnchainSparseMerkleTreeProof2023)); proofI != nil {

	} else {
		return out, errProofNotFound(verifiable.Iden3SparseMerkleTreeProofType)
	}

	issuerID, err := core.IDFromDID(*issuerDID)
	if err != nil {
		return out, err
	}
	out.IssuerID = &issuerID

	out.Claim, err = proofI.GetCoreClaim()
	if err != nil {
		return out, err
	}

	if claimProcessFn != nil {
		claimProcessFn(out.Claim)
	}

	credStatus, ok := w3cCred.CredentialStatus.(jsonObj)
	if !ok {
		return out, errors.New("not a json object")
	}

	out.NonRevProof, err = buildAndValidateCredentialStatus(ctx, cfg,
		credStatus, out.IssuerID, skipClaimRevocationCheck)
	if err != nil {
		return out, err
	}

	return out, nil
}

func v3ProofFromMTP(
	p circuits.ClaimWithMTPProof) circuits.ClaimWithSigAndMTPProof {
	return circuits.ClaimWithSigAndMTPProof{
		IssuerID:    p.IssuerID,
		Claim:       p.Claim,
		NonRevProof: p.NonRevProof,
		IncProof:    &p.IncProof,
	}
}

func v3ProofFromSig(p circuits.ClaimWithSigProof) circuits.ClaimWithSigAndMTPProof {
	return circuits.ClaimWithSigAndMTPProof{
		IssuerID:       p.IssuerID,
		Claim:          p.Claim,
		NonRevProof:    p.NonRevProof,
		SignatureProof: &p.SignatureProof,
	}

}

func claimWithSigAndMtpProofFromObj(ctx context.Context, cfg EnvConfig,
	w3cCred verifiable.W3CCredential, skipClaimRevocationCheck bool,
	proofType circuits.ProofType) (circuits.ClaimWithSigAndMTPProof, circuits.ProofType, error) {

	switch proofType {
	case circuits.Iden3SparseMerkleTreeProofType:
		claimWithMtpProof, err := claimWithMtpProofFromObj(ctx, cfg, w3cCred,
			skipClaimRevocationCheck, nil)
		if err != nil {
			return circuits.ClaimWithSigAndMTPProof{}, proofType, err
		}
		return v3ProofFromMTP(claimWithMtpProof), proofType, nil
	case circuits.BJJSignatureProofType:
		claimWithSigProof, err := claimWithSigProofFromObj(ctx, cfg, w3cCred,
			skipClaimRevocationCheck)
		if err != nil {
			return circuits.ClaimWithSigAndMTPProof{}, proofType, err
		}
		return v3ProofFromSig(claimWithSigProof), proofType, nil
	case "":
		claimWithMtpProof, err := claimWithMtpProofFromObj(ctx, cfg, w3cCred,
			skipClaimRevocationCheck, nil)
		var tErr errProofNotFound
		switch {
		case errors.As(err, &tErr):
			claimWithSigProof, err := claimWithSigProofFromObj(ctx, cfg,
				w3cCred, skipClaimRevocationCheck)
			if err != nil {
				return circuits.ClaimWithSigAndMTPProof{}, proofType, err
			}
			return v3ProofFromSig(claimWithSigProof),
				circuits.BJJSignatureProofType, nil
		case err != nil:
			return circuits.ClaimWithSigAndMTPProof{}, proofType, err
		}

		return v3ProofFromMTP(claimWithMtpProof),
			circuits.Iden3SparseMerkleTreeProofType, nil
	default:
		return circuits.ClaimWithSigAndMTPProof{}, proofType,
			fmt.Errorf("unknown proofType: %v", proofType)
	}
}

func circuitsTreeStateFromSchemaState(
	state verifiable.State) (ts circuits.TreeState, err error) {

	if state.Value == nil {
		return ts, errors.New("state value is nil")
	}
	ts.State, err = merkletree.NewHashFromHex(*state.Value)
	if err != nil {
		return ts, err
	}
	if state.ClaimsTreeRoot == nil {
		return ts, errors.New("state claims tree root is nil")
	}
	ts.ClaimsRoot, err = merkletree.NewHashFromHex(*state.ClaimsTreeRoot)
	if err != nil {
		return ts, err
	}
	if state.RevocationTreeRoot != nil {
		ts.RevocationRoot, err =
			merkletree.NewHashFromHex(*state.RevocationTreeRoot)
		if err != nil {
			return ts, err
		}
	} else {
		ts.RevocationRoot = &merkletree.Hash{}
	}
	if state.RootOfRoots != nil {
		ts.RootOfRoots, err = merkletree.NewHashFromHex(*state.RootOfRoots)
		if err != nil {
			return ts, err
		}
	} else {
		ts.RootOfRoots = &merkletree.Hash{}
	}
	return
}

var supportedCredentialStatusTypes = map[verifiable.CredentialStatusType]bool{
	verifiable.Iden3ReverseSparseMerkleTreeProof:     true,
	verifiable.SparseMerkleTreeProof:                 true,
	verifiable.Iden3OnchainSparseMerkleTreeProof2023: true,
}

func resolveRevStatus(ctx context.Context, cfg EnvConfig,
	credStatus interface{}, issuerID *core.ID) (circuits.MTProof, error) {

	switch status := credStatus.(type) {
	case *verifiable.CredentialStatus:
		if status.Type == verifiable.Iden3ReverseSparseMerkleTreeProof {
			revNonce := new(big.Int).SetUint64(status.RevocationNonce)
			return resolveRevStatusFromRHS(ctx, status.ID, cfg, issuerID,
				revNonce)
		}
		if status.Type == verifiable.Iden3OnchainSparseMerkleTreeProof2023 {
			return resolverOnChainRevocationStatus(ctx, cfg, issuerID, status)
		}
		return resolveRevocationStatusFromIssuerService(ctx, status.ID)

	case verifiable.CredentialStatus:
		return resolveRevStatus(ctx, cfg, &status, issuerID)

	case jsonObj:
		credStatusType, ok := status["type"].(string)
		if !ok {
			return circuits.MTProof{},
				errors.New("credential status doesn't contain type")
		}
		credentialStatusType := verifiable.CredentialStatusType(credStatusType)
		if !supportedCredentialStatusTypes[credentialStatusType] {
			return circuits.MTProof{}, fmt.Errorf(
				"credential status type %s id not supported",
				credStatusType)
		}

		var typedCredentialStatus verifiable.CredentialStatus
		err := remarshalObj(&typedCredentialStatus, status)
		if err != nil {
			return circuits.MTProof{}, err
		}
		return resolveRevStatus(ctx, cfg, &typedCredentialStatus, issuerID)

	default:
		return circuits.MTProof{},
			errors.New("unknown credential status format")
	}
}

// marshal/unmarshal object from one type to ther
func remarshalObj(dst, src any) error {
	objBytes, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(objBytes, dst)
}

type stateContractIDKey struct {
	contractAddr common.Address
	id           core.ID
}

var idsInStateContract = map[stateContractIDKey]bool{}
var idsInStateContractLock sync.RWMutex

func stateContractHasID(ctx context.Context, id *core.ID, cfg ChainConfig,
	cli bind.ContractCaller) (bool, error) {
	key := stateContractIDKey{
		contractAddr: cfg.StateContractAddr,
		id:           *id,
	}

	idsInStateContractLock.RLock()
	ok := idsInStateContract[key]
	idsInStateContractLock.RUnlock()
	if ok {
		return ok, nil
	}

	idsInStateContractLock.Lock()
	defer idsInStateContractLock.Unlock()

	ok = idsInStateContract[key]
	if ok {
		return ok, nil
	}

	_, err := lastStateFromContractWithClient(ctx, cfg, id, cli)
	if errors.Is(err, errIdentityDoesNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	idsInStateContract[key] = true
	return true, err
}

type onchainRevStatus struct {
	chainID         ChainID
	contractAddress common.Address
	revNonce        uint64
	genesisState    *big.Int
}

func newOnchainRevStatusFromURI(stateID string) (onchainRevStatus, error) {
	var s onchainRevStatus

	uri, err := url.Parse(stateID)
	if err != nil {
		return s, errors.New("OnChainCredentialStatus ID is not a valid URI")
	}

	contract := uri.Query().Get("contractAddress")
	if contract == "" {
		return s, errors.New("OnChainCredentialStatus contract address is empty")
	}

	contractParts := strings.Split(contract, ":")
	if len(contractParts) != 2 {
		return s, errors.New(
			"OnChainCredentialStatus contract address is not valid")
	}

	s.chainID, err = newChainIDFromString(contractParts[0])
	if err != nil {
		return s, err
	}

	if !common.IsHexAddress(contractParts[1]) {
		return s, errors.New(
			"OnChainCredentialStatus incorrect contract address")
	}
	s.contractAddress = common.HexToAddress(contractParts[1])

	revocationNonce := uri.Query().Get("revocationNonce")
	if revocationNonce == "" {
		return s, errors.New("revocationNonce is empty in OnChainCredentialStatus ID")
	}

	s.revNonce, err = strconv.ParseUint(revocationNonce, 10, 64)
	if err != nil {
		return s, errors.New("revocationNonce is not a number in OnChainCredentialStatus ID")
	}

	// state may be nil if params is absent in query
	s.genesisState, err = newIntFromHexQueryParam(uri, "state")
	if err != nil {
		return s, err
	}

	return s, nil
}

// Currently, our library does not have a Close function. As a result, we
// create and destroy an Ethereum client for each usage of this function.
// Although this approach may be inefficient, it is acceptable if the function
// is rarely called. If this becomes an issue in the future, or if a Close
// function is implemented, we will need to refactor this function to use a
// global Ethereum client.
func resolverOnChainRevocationStatus(ctx context.Context, cfg EnvConfig,
	id *core.ID,
	status *verifiable.CredentialStatus) (circuits.MTProof, error) {

	var zeroID core.ID
	if id == nil || *id == zeroID {
		return circuits.MTProof{}, errors.New("issuer ID is empty")
	}

	onchainRevStatus, err := newOnchainRevStatusFromURI(status.ID)
	if err != nil {
		return circuits.MTProof{}, err
	}

	if onchainRevStatus.revNonce != status.RevocationNonce {
		return circuits.MTProof{}, fmt.Errorf(
			"revocationNonce is not equal to the one "+
				"in OnChainCredentialStatus ID {%d} {%d}",
			onchainRevStatus.revNonce, status.RevocationNonce)
	}

	networkCfg, err := cfg.networkCfgByChainID(onchainRevStatus.chainID)
	if err != nil {
		return circuits.MTProof{}, err
	}

	client, err := ethclient.Dial(networkCfg.RPCUrl)
	if err != nil {
		return circuits.MTProof{}, err
	}
	defer client.Close()

	contractCaller, err := onchainABI.NewOnchainCredentialStatusResolverCaller(
		onchainRevStatus.contractAddress, client)
	if err != nil {
		return circuits.MTProof{}, err
	}

	isStateContractHasID, err := stateContractHasID(ctx, id, networkCfg, client)
	if err != nil {
		return circuits.MTProof{}, err
	}

	opts := &bind.CallOpts{Context: ctx}
	var resp onchainABI.IOnchainCredentialStatusResolverCredentialStatus
	if isStateContractHasID {
		// TODO: it is not finial version of contract GetRevocationProof must accept issuer id as parameter
		resp, err = contractCaller.GetRevocationStatus(opts, id.BigInt(),
			onchainRevStatus.revNonce)
		if err != nil {
			msg := err.Error()
			if isErrInvalidRootsLength(err) {
				msg = "roots were not saved to identity tree store"
			}
			return circuits.MTProof{}, fmt.Errorf(
				"GetRevocationProof smart contract call [GetRevocationStatus]: %s",
				msg)
		}
	} else {
		if onchainRevStatus.genesisState == nil {
			return circuits.MTProof{}, errors.New(
				"genesis state is not specified in OnChainCredentialStatus ID")
		}
		resp, err = contractCaller.GetRevocationStatusByIdAndState(opts,
			id.BigInt(), onchainRevStatus.genesisState,
			onchainRevStatus.revNonce)
		if err != nil {
			return circuits.MTProof{}, fmt.Errorf(
				"GetRevocationProof smart contract call [GetRevocationStatusByIdAndState]: %s",
				err.Error())
		}
	}

	return toMerkleTreeProof(resp)
}

func toMerkleTreeProof(status onchainABI.IOnchainCredentialStatusResolverCredentialStatus) (circuits.MTProof, error) {
	var existence bool
	var nodeAux *merkletree.NodeAux
	var err error

	if status.Mtp.Existence {
		existence = true
	} else {
		existence = false
		if status.Mtp.AuxExistence {
			nodeAux = &merkletree.NodeAux{}
			nodeAux.Key, err = merkletree.NewHashFromBigInt(status.Mtp.AuxIndex)
			if err != nil {
				return circuits.MTProof{}, errors.New("aux index is not a number")
			}
			nodeAux.Value, err = merkletree.NewHashFromBigInt(status.Mtp.AuxValue)
			if err != nil {
				return circuits.MTProof{}, errors.New("aux value is not a number")
			}
		}
	}

	//allSiblings := make([]*merkletree.Hash, len(status.Mtp.Siblings))
	depth := calculateDepth(status.Mtp.Siblings)
	allSiblings := make([]*merkletree.Hash, depth)
	for i := 0; i < depth; i++ {
		sh, err2 := merkletree.NewHashFromBigInt(status.Mtp.Siblings[i])
		if err2 != nil {
			return circuits.MTProof{}, errors.New("sibling is not a number")
		}
		allSiblings[i] = sh
	}

	proof, err := merkletree.NewProofFromData(existence, allSiblings, nodeAux)
	if err != nil {
		return circuits.MTProof{}, errors.New("failed to create proof")
	}

	state, err := merkletree.NewHashFromBigInt(status.Issuer.State)
	if err != nil {
		return circuits.MTProof{}, errors.New("state is not a number")
	}

	claimsRoot, err := merkletree.NewHashFromBigInt(status.Issuer.ClaimsTreeRoot)
	if err != nil {
		return circuits.MTProof{}, errors.New("state is not a number")
	}

	revocationRoot, err := merkletree.NewHashFromBigInt(status.Issuer.RevocationTreeRoot)
	if err != nil {
		return circuits.MTProof{}, errors.New("state is not a number")
	}

	rootOfRoots, err := merkletree.NewHashFromBigInt(status.Issuer.RootOfRoots)
	if err != nil {
		return circuits.MTProof{}, errors.New("state is not a number")
	}

	return circuits.MTProof{
		Proof: proof,
		TreeState: circuits.TreeState{
			State:          state,
			ClaimsRoot:     claimsRoot,
			RevocationRoot: revocationRoot,
			RootOfRoots:    rootOfRoots,
		},
	}, nil
}

func calculateDepth(siblings []*big.Int) int {
	for i := len(siblings) - 1; i >= 0; i-- {
		if siblings[i].Cmp(big.NewInt(0)) != 0 {
			return i + 1
		}
	}
	return 0
}

type ChainConfig struct {
	RPCUrl            string
	StateContractAddr common.Address
}

func (cc ChainConfig) validate() error {
	if cc.RPCUrl == "" {
		return errors.New("ethereum url is empty")
	}

	if cc.StateContractAddr == (common.Address{}) {
		return errors.New("contract address is empty")
	}

	return nil
}

type PerChainConfig map[ChainID]ChainConfig

func (p *PerChainConfig) UnmarshalJSON(bytes []byte) error {
	if (*p) == nil {
		*p = make(PerChainConfig)
	}
	var o map[string]ChainConfig
	err := json.Unmarshal(bytes, &o)
	if err != nil {
		return err
	}
	for k, v := range o {
		var chainID ChainID
		chainID, err = newChainIDFromString(k)
		if err != nil {
			return err
		}
		(*p)[chainID] = v
	}
	return nil
}

type EnvConfig struct {
	ChainConfigs          PerChainConfig
	EthereumURL           string         // Deprecated: Use ChainConfigs instead
	StateContractAddr     common.Address // Deprecated: Use ChainConfigs instead
	ReverseHashServiceUrl string         // Deprecated
	IPFSNodeURL           string
}

func (cfg EnvConfig) documentLoader() ld.DocumentLoader {
	var ipfsNode loaders.IPFSClient
	if cfg.IPFSNodeURL != "" {
		ipfsNode = &ipfsCli{rpcURL: cfg.IPFSNodeURL}
	}

	var opts []loaders.DocumentLoaderOption

	cacheEngine, err := newBadgerCacheEngine(
		withEmbeddedDocumentBytes(
			"https://www.w3.org/2018/credentials/v1",
			credentialsV1JsonLDBytes))
	if err == nil {
		opts = append(opts, loaders.WithCacheEngine(cacheEngine))
	}

	return loaders.NewDocumentLoader(ipfsNode, "", opts...)
}

type ChainID uint64

func (cid ChainID) Unpack() (core.Blockchain, core.NetworkID, error) {
	for k, v := range knownChainIDs {
		if v == cid {
			return k.blockchain, k.networkID, nil
		}
	}
	return core.NoChain, core.NoNetwork, fmt.Errorf("unknown chain ID")
}

func newChainIDFromString(in string) (ChainID, error) {
	var chainID uint64
	var err error
	if strings.HasPrefix(in, "0x") ||
		strings.HasPrefix(in, "0X") {
		chainID, err = strconv.ParseUint(in[2:], 16, 64)
		if err != nil {
			return 0, err
		}
	} else {
		chainID, err = strconv.ParseUint(in, 10, 64)
		if err != nil {
			return 0, err
		}
	}
	return ChainID(chainID), nil
}

type chainIDKey struct {
	blockchain core.Blockchain
	networkID  core.NetworkID
}

var knownChainIDs = map[chainIDKey]ChainID{
	{core.Ethereum, core.Main}:    1,
	{core.Ethereum, core.Goerli}:  5,
	{core.Polygon, core.Main}:     137,
	{core.ZkEVM, core.Main}:       1101,
	{core.ZkEVM, core.Test}:       1442,
	{core.Polygon, core.Mumbai}:   80001,
	{core.Ethereum, core.Sepolia}: 11155111,
}

func (cfg EnvConfig) networkCfgByID(id *core.ID) (ChainConfig, error) {
	chainID, err := chainIDFromID(id)
	if err != nil {
		return ChainConfig{}, err
	}

	return cfg.networkCfgByChainID(chainID)
}

func (cfg EnvConfig) networkCfgByChainID(chainID ChainID) (ChainConfig, error) {
	chainCfg, ok := cfg.ChainConfigs[chainID]
	if !ok {
		chainCfg = cfg.defaultChainCfg()
	}

	return chainCfg, chainCfg.validate()
}

func (cfg EnvConfig) defaultChainCfg() ChainConfig {
	return ChainConfig{
		RPCUrl:            cfg.EthereumURL,
		StateContractAddr: cfg.StateContractAddr,
	}
}

func chainIDFromID(id *core.ID) (ChainID, error) {
	blockchain, err := core.BlockchainFromID(*id)
	if err != nil {
		return 0, err
	}

	networkID, err := core.NetworkIDFromID(*id)
	if err != nil {
		return 0, err
	}

	key := chainIDKey{blockchain, networkID}
	chainID, ok := knownChainIDs[key]
	if !ok {
		return 0, fmt.Errorf("unknown chain: %s", id.String())
	}

	return chainID, nil
}

// Currently, our library does not have a Close function. As a result, we
// create and destroy an Ethereum client for each usage of this function.
// Although this approach may be inefficient, it is acceptable if the function
// is rarely called. If this becomes an issue in the future, or if a Close
// function is implemented, we will need to refactor this function to use a
// global Ethereum client.
func lastStateFromContract(ctx context.Context, cfg EnvConfig,
	id *core.ID) (*merkletree.Hash, error) {

	networkCfg, err := cfg.networkCfgByID(id)
	if err != nil {
		return nil, err
	}

	var zeroID core.ID
	if id == nil || *id == zeroID {
		return nil, errors.New("ID is empty")
	}

	client, err := ethclient.Dial(networkCfg.RPCUrl)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	return lastStateFromContractWithClient(ctx, networkCfg, id, client)
}

func lastStateFromContractWithClient(ctx context.Context, cfg ChainConfig,
	id *core.ID, cli bind.ContractCaller) (*merkletree.Hash, error) {

	contractCaller, err := abi.NewStateCaller(cfg.StateContractAddr, cli)
	if err != nil {
		return nil, err
	}

	opts := &bind.CallOpts{Context: ctx}
	resp, err := contractCaller.GetStateInfoById(opts, id.BigInt())
	if isErrIdentityDoesNotExist(err) {
		return nil, errIdentityDoesNotExist
	} else if err != nil {
		return nil, err
	}

	if resp.State == nil {
		return nil, errors.New("got nil state from contract")
	}

	return merkletree.NewHashFromBigInt(resp.State)
}

var errIdentityDoesNotExist = errors.New("identity does not exist")

func isErrIdentityDoesNotExist(err error) bool {
	rpcErr, isRPCErr := err.(rpc.Error)
	if !isRPCErr {
		return false
	}
	return rpcErr.ErrorCode() == 3 &&
		rpcErr.Error() == "execution reverted: Identity does not exist"
}

func isErrInvalidRootsLength(err error) bool {
	rpcErr, isRPCErr := err.(rpc.Error)
	if !isRPCErr {
		return false
	}
	return rpcErr.ErrorCode() == 3 &&
		rpcErr.Error() == "execution reverted: Invalid roots length"
}

func newRhsCli(rhsURL string) (mp.ReverseHashCli, error) {
	if rhsURL == "" {
		return nil, errors.New("reverse hash service url is empty")
	}

	return &mpHttp.ReverseHashCli{
		URL:         rhsURL,
		HTTPTimeout: 10 * time.Second,
	}, nil
}

func treeStateFromRHS(ctx context.Context, rhsCli mp.ReverseHashCli,
	state *merkletree.Hash) (circuits.TreeState, error) {

	var treeState circuits.TreeState

	stateNode, err := rhsCli.GetNode(ctx, state)
	if err != nil {
		return treeState, err
	}

	if len(stateNode.Children) != 3 {
		return treeState, errors.New(
			"invalid state node, should have 3 children")
	}

	treeState.State = state
	treeState.ClaimsRoot = stateNode.Children[0]
	treeState.RevocationRoot = stateNode.Children[1]
	treeState.RootOfRoots = stateNode.Children[2]

	return treeState, err
}

func identityStateForRHS(ctx context.Context, cfg EnvConfig, issuerID *core.ID,
	genesisState *merkletree.Hash) (*merkletree.Hash, error) {

	state, err := lastStateFromContract(ctx, cfg, issuerID)
	if !errors.Is(err, errIdentityDoesNotExist) {
		return state, err
	}

	if genesisState == nil {
		return nil, errors.New("current state is not found for the identity")
	}

	stateIsGenesis, err := genesisStateMatch(genesisState, *issuerID)
	if err != nil {
		return nil, err
	}

	if !stateIsGenesis {
		return nil, errors.New("state is not genesis for the identity")
	}

	return genesisState, nil
}

// check if genesis state matches the state from the ID
func genesisStateMatch(state *merkletree.Hash, id core.ID) (bool, error) {
	var tp [2]byte
	copy(tp[:], id[:2])
	otherID, err := core.NewIDFromIdenState(tp, state.BigInt())
	if err != nil {
		return false, err
	}
	return bytes.Equal(otherID[:], id[:]), nil
}

func rhsBaseURL(rhsURL string) (string, *merkletree.Hash, error) {
	u, err := url.Parse(rhsURL)
	if err != nil {
		return "", nil, err
	}
	var state *merkletree.Hash
	stateStr := u.Query().Get("state")
	if stateStr != "" {
		state, err = merkletree.NewHashFromHex(stateStr)
		if err != nil {
			return "", nil, err
		}
	}

	if strings.HasSuffix(u.Path, "/node") {
		u.Path = strings.TrimSuffix(u.Path, "node")
	}
	if strings.HasSuffix(u.Path, "/node/") {
		u.Path = strings.TrimSuffix(u.Path, "node/")
	}

	u.RawQuery = ""
	return u.String(), state, nil
}

func resolveRevStatusFromRHS(ctx context.Context, rhsURL string, cfg EnvConfig,
	issuerID *core.ID, revNonce *big.Int) (circuits.MTProof, error) {

	var p circuits.MTProof

	baseRHSURL, genesisState, err := rhsBaseURL(rhsURL)
	if err != nil {
		return p, err
	}

	state, err := identityStateForRHS(ctx, cfg, issuerID, genesisState)
	if err != nil {
		return p, err
	}

	rhsCli, err := newRhsCli(baseRHSURL)
	if err != nil {
		return p, err
	}

	p.TreeState, err = treeStateFromRHS(ctx, rhsCli, state)
	if errors.Is(err, mp.ErrNodeNotFound) {
		if genesisState != nil && state.Equals(genesisState) {
			return p, errors.New("genesis state is not found in RHS")
		} else {
			return p, errors.New("current state is not found in RHS")
		}
	} else if err != nil {
		return p, err
	}

	revNonceHash, err := merkletree.NewHashFromBigInt(revNonce)
	if err != nil {
		return p, err
	}

	p.Proof, err = rhsCli.GenerateProof(ctx, p.TreeState.RevocationRoot,
		revNonceHash)
	if err != nil {
		return p, err
	}

	return p, nil
}

func newIntFromBytesLE(bs []byte) *big.Int {
	return new(big.Int).SetBytes(utils.SwapEndianness(bs))
}

// newIntFromHexQueryParam search for query param `paramName`, parse it
// as hex string of LE bytes of *big.Int. Return nil if param is not found.
func newIntFromHexQueryParam(uri *url.URL, paramName string) (*big.Int, error) {
	stateParam := uri.Query().Get(paramName)
	if stateParam == "" {
		return nil, nil
	}

	stateParam = strings.TrimSuffix(stateParam, "0x")
	stateBytes, err := hex.DecodeString(stateParam)
	if err != nil {
		return nil, err
	}

	return newIntFromBytesLE(stateBytes), nil
}

func merklizeCred(ctx context.Context, w3cCred verifiable.W3CCredential,
	documentLoader ld.DocumentLoader,
	ignoreCacheErrors bool) (*merklize.Merklizer, error) {

	w3cCred.Proof = nil
	credentialBytes, err := json.Marshal(w3cCred)
	if err != nil {
		return nil, err
	}

	cacheKey := sha256.Sum256(credentialBytes)

	db, cleanup, err := getCacheDB()
	if err != nil {
		if !ignoreCacheErrors {
			return nil, err
		}
		slog.ErrorContext(ctx, "failed to get cache db", "err", err)
		db = nil
	} else {
		defer cleanup()
	}

	var mz *merklize.Merklizer
	var storage *inMemoryStorage

	if db != nil {
		mz, storage, err = getMzCache(ctx, db, cacheKey[:], documentLoader)
		if err != nil {
			if !ignoreCacheErrors {
				return nil, err
			}
			slog.ErrorContext(ctx, "failed to read value from cache db",
				"err", err)
			mz = nil
			storage = nil
		}
	}

	if mz == nil || storage == nil {
		slog.Debug("merklizeCred: cache miss")
		storage = newInMemoryStorage()

		var mt *merkletree.MerkleTree
		mt, err = merkletree.NewMerkleTree(ctx, storage, mtLevels)
		if err != nil {
			return nil, err
		}

		warmUpSchemaLoader(w3cCred.Context, documentLoader)

		mz, err = merklize.MerklizeJSONLD(ctx, bytes.NewReader(credentialBytes),
			merklize.WithDocumentLoader(documentLoader),
			merklize.WithMerkleTree(merklize.MerkleTreeSQLAdapter(mt)))
		if err != nil {
			return nil, err
		}

		if db != nil {
			err = saveMzCache(db, cacheKey[:], mz, storage)
			if err != nil {
				if !ignoreCacheErrors {
					return nil, err
				}
				slog.ErrorContext(ctx, "failed to save to the cache db",
					"err", err)
			}
		}
	} else {
		slog.Debug("merklizeCred: cache hit")
	}

	return mz, nil
}

func mzCacheKey(vcChecksum []byte) []byte {
	return appendSuffix("_mz", vcChecksum)
}

func storageCacheKey(vcChecksum []byte) []byte {
	return appendSuffix("_mt", vcChecksum)
}

func appendSuffix(suffix string, val []byte) []byte {
	newVal := make([]byte, len(val)+len(suffix))
	copy(newVal, val)
	copy(newVal[len(val):], suffix)
	return newVal
}

func saveMzCache(db *badger.DB, vcChecksum []byte, mz *merklize.Merklizer,
	storage *inMemoryStorage) error {

	expireAt := time.Now().Add(30 * 24 * time.Hour).Unix()

	var storageEntry = badger.Entry{
		Key:       storageCacheKey(vcChecksum),
		ExpiresAt: uint64(expireAt)}
	var mzEntry = badger.Entry{
		Key:       mzCacheKey(vcChecksum),
		ExpiresAt: uint64(expireAt)}

	var err error
	storageEntry.Value, err = storage.MarshalBinary()
	if err != nil {
		return fmt.Errorf("failed to marshal storage: %w", err)
	}

	mzEntry.Value, err = mz.MarshalBinary()
	if err != nil {
		return fmt.Errorf("failed to marshal merklizer: %w", err)
	}

	err = db.Update(func(txn *badger.Txn) error {
		if err := txn.SetEntry(&storageEntry); err != nil {
			return err
		}
		return txn.SetEntry(&mzEntry)
	})
	if err != nil {
		return fmt.Errorf("failed to save value to cache db: %w", err)
	}
	return nil
}

//func logWhatWasPutInCache(msg string, key []byte, value []byte) {
//	valueHex := hex.EncodeToString(value)
//	checksum := md5.Sum(value)
//	checksumHex := hex.EncodeToString(checksum[:])
//	keyHex := hex.EncodeToString(key)
//	slog.Debug(msg, "key", keyHex, "len", len(value),
//		"md5", checksumHex, "value", valueHex)
//}

func getMzCache(ctx context.Context, db *badger.DB, vcChecksum []byte,
	documentLoader ld.DocumentLoader) (*merklize.Merklizer, *inMemoryStorage,
	error) {

	var mz *merklize.Merklizer
	var storage *inMemoryStorage

	err := db.View(func(txn *badger.Txn) error {
		mtKey := storageCacheKey(vcChecksum)
		v, err := txn.Get(mtKey)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		} else if err != nil {
			return err
		}

		storage = newInMemoryStorage()
		err = v.Value(func(val []byte) error {
			err = storage.UnmarshalBinary(val)
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}

		mzKey := mzCacheKey(vcChecksum)
		v, err = txn.Get(mzKey)
		if errors.Is(err, badger.ErrKeyNotFound) {
			return errors.New("merklized data not found in cache db")
		} else if err != nil {
			return err
		}

		var mt *merkletree.MerkleTree
		mt, err = merkletree.NewMerkleTree(ctx, storage, mtLevels)
		if err != nil {
			return err
		}

		return v.Value(func(val []byte) error {
			mz, err = merklize.MerklizerFromBytes(val,
				merklize.WithDocumentLoader(documentLoader),
				merklize.WithMerkleTree(merklize.MerkleTreeSQLAdapter(mt)))
			return err
		})
	})

	return mz, storage, err
}

func warmUpSchemaLoader(schemaURLs []string, docLoader ld.DocumentLoader) {
	var wg sync.WaitGroup
	start := time.Now()
	for _, schemaURL := range schemaURLs {
		wg.Add(1)
		go func(schemaURL string) {
			defer wg.Done()
			_, _ = docLoader.LoadDocument(schemaURL)
		}(schemaURL)
	}
	wg.Wait()
	slog.Debug("pre download schemas",
		"time", time.Since(start),
		"docsNum", len(schemaURLs))
}

func PreCacheVC(ctx context.Context, cfg EnvConfig, in []byte) error {
	var obj struct {
		VerifiableCredentials json.RawMessage `json:"verifiableCredentials"`
	}
	err := json.Unmarshal(in, &obj)
	if err != nil {
		return err
	}

	var w3cCred verifiable.W3CCredential
	err = json.Unmarshal(obj.VerifiableCredentials, &w3cCred)
	if err != nil {
		return err
	}

	_, err = merklizeCred(ctx, w3cCred, cfg.documentLoader(), false)
	return err
}

func fmtPath(path merklize.Path) string {
	var parts []string
	for _, p := range path.Parts() {
		parts = append(parts, fmt.Sprintf("%v", p))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func verifierIDFromTxData(txData txData) (*core.ID, error) {
	genState := core.GenesisFromEthAddress(txData.ContractAddress)
	blockchain, networkID, err := txData.ChainID.Unpack()
	if err != nil {
		return nil, err
	}
	tp, err := core.BuildDIDType(core.DIDMethodIden3, blockchain, networkID)
	if err != nil {
		return nil, err
	}
	id := core.NewID(tp, genState)
	return &id, nil
}
