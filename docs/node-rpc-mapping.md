# Node JSON-RPC to Blockbook API mapping

Consumers that normally talk to a Bitcoin-type node's JSON-RPC interface can
serve the same needs from the Blockbook public API. This page maps the common
node methods to their Blockbook equivalents. Full endpoint documentation lives
in the [OpenAPI specification](../openapi.yaml) (served at `/api-docs/`).

| Node JSON-RPC | Blockbook equivalent |
| --- | --- |
| `getblockhash <height>` | `GET /api/v2/block-index/{height}` (WS `getBlockHash`) |
| `getblockcount` | `GET /api/` → `blockbook.bestHeight` (Blockbook index tip) or `backend.blocks` (node tip); WS `getInfo` |
| `getblock <hash\|height>` | `GET /api/v2/block/{blockId}` (parsed, paged txs); raw hex via `GET /api/v2/rawblock/{blockId}` |
| `getrawtransaction <txid> 1` | `GET /api/v2/tx/{txid}` (normalized) or `GET /api/v2/tx-specific/{txid}` (node-native verbose JSON) |
| `getrawtransaction <txid> 0` | `GET /api/v2/tx/{txid}` → `.hex` |
| `sendrawtransaction <hex>` | `POST /api/v2/sendtx/` (body = hex) or `GET /api/v2/sendtx/{hex}`; WS `sendTransaction` |
| `estimatefee <blocks>` | `GET /api/v2/estimatefee/{blocks}`; WS `estimateFee` |
| `getrawmempool` | `GET /api/v2/mempool/` (paged, includes first-seen times) |
| `gettxout <txid> <n>` | recipe below |

## gettxout recipe

There is no dedicated single-outpoint endpoint; the data is already available
from two existing calls.

1. `GET /api/v2/tx/{txid}` and read `vout[n]`:
   - `value` — output amount in base units.
   - `hex` — the scriptPubKey.
   - `addresses` / `isAddress` — decoded output addresses.
   - `spent` (with `spentTxId`/`spentHeight` on instances running with
     `-extendedindex`) — set when the output is spent by a **confirmed**
     transaction.
   - top-level `confirmations` — 0 when the transaction is still in the
     mempool, so outputs created by mempool transactions are visible.
   - `vin[0].coinbase` — non-empty when the transaction is a coinbase
     (mind coinbase maturity before treating the output as spendable).
2. Only when `spent` is false and mempool awareness is required (bitcoind's
   `include_mempool=true` behavior — an output already spent by an unconfirmed
   transaction should be treated as gone): `GET /api/v2/utxo/{address}` for one
   of the output's addresses (mempool-aware by default) and check whether
   `txid:n` is present in the returned list.
   - present → unspent, including mempool view (`confirmations` 0 = the UTXO
     itself is unconfirmed);
   - absent → spent by a mempool transaction.

Equivalence to bitcoind: `gettxout` returning `null` corresponds to either a
404/400 error from `tx/{txid}` (unknown transaction), a `vout` index out of
range, `spent: true`, or the outpoint missing from the address UTXO set.

## Mempool notes

`GET /api/v2/mempool/` returns the mempool view of this Blockbook instance:
entries and first-seen timestamps can differ between instances and from the
backend node. To maintain a live view, bootstrap with `GET /api/v2/mempool/`
and keep it current with the WebSocket `subscribeNewTransaction` subscription;
`blockbook.mempoolSize` from `GET /api/` is a cheap consistency check.
