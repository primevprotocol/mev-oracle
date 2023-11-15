# Oracle
Connects to the Oracle contract to trigger rewards and slashing of commitments.

## Usage

The main utility has several flags:

- contract: This flag is used to specify the contract address of the oracle. The default value is "0xCf7Ed3AccA5a467e9e704C703E8D87F634fB0Fc9".

- url: This flag is used to specify the URL of the settlement layer for an RPC connection. The default value is "http://localhost:8545" to be used with anvil.

- key: This flag is used to specify the private key through which interactions are signed. The default value is "ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80". Make sure the key is funded sufficently.

- chainID: This flag is used to specify the chain ID of the settlement layer. The default value is 31337. TODO(@ckartik): can get from rpc connection.

- rateLimit: The time to wait before querying 3rd party services for more data.

These flags can be overridden by providing new values when running the program. For example:

`go run main.go -contract=NewContractAddress -url=NewClientURL -key=NewPrivateKey -chainID=NewChainID  --rateLimit=5`



## Impelementation

The core part of this service is the chain tracer, which has the following interface:
```go
type Tracer interface {
	IncrementBlock() (NewBlockNumber int64)
	RetrieveDetails() (block *BlockDetails, BlockBuilder string, err error)
}
```

There are two implementations:
- DummyTracer
    - This is fed random data and can be changed to custom data for e2e testing
- IncrementingTracer
    - This is fed data from Infura (txns in block) and PayloadsDe (builder that won)

## Open Concerns on Oracle
- We need to have a reliable way of determining the winning builder.
