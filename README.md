# libre3sim

A FreeStyle Libre 3 sensor emulator, for exercising LibreLoop and RoundWhiteDiscKit
without a real sensor. It serves an app in the iOS Simulator through
[ImpossiBLE](https://github.com/mickeyl/ImpossiBLE), the same way
[g7sim](https://github.com/LoopKit/g7sim) does for the Dexcom G7.

> **For development use only — not an Abbott product.** libre3sim is an independent,
> unofficial tool for testing CGM software. It is not made, endorsed, or supported by
> Abbott, it is not a medical device, and it emits only simulated, made-up data.
> "FreeStyle Libre" and "Libre" are trademarks of Abbott, used here solely to describe
> interoperability. Never use it for therapy or any clinical decision.

## What it emulates

| Part | State |
|---|---|
| NFC patch info (`a1`), activation (`a0`), switch receiver (`a8`, with the `b1` receiver check; a terminated sensor answers `a5 01 b0`, as a real one did) | done, unit tested |
| Command-gated handshake: certificates, ephemeral keys, kAuth, Phase 5/6 | done, unit tested |
| Cached reconnect from `StartAuthorization` with the saved kAuth | done, unit tested |
| BLE PIN burned by each NFC scan; a full handshake needs a scan first | done, unit tested |
| Data plane: patch status and realtime glucose each minute, AES-CCM as captured | done; frames match RoundWhiteDiscKit's captures byte for byte |
| Historical and clinical backfill on `patchControl`; clinical replay on resubscribe | done, unit tested |
| Warmup, end of wear, early termination (`errorData` 6/8/3), shutdown command | done |
| Staged link drops and outages | done |
| iOS Simulator transport (ImpossiBLE), state restoration, suspension | from g7sim |
| A Raspberry Pi BLE transport | not yet |

The sensor model follows the client side in RoundWhiteDiscKit (`PairingFlow`,
`SensorSession`, the `DataPlane` decoders) and LibreLoop's sensor monitor. Where those do
not pin a behaviour down, libre3sim picks one and says so in the code: the NFC state byte
during warmup (`04`, as when running), the patch state during warmup (`4`), the data-plane descriptors for the
clinical, event-log and factory channels, and what a sensor does with a Phase 5 it cannot
verify (it drops the link).

## Trust

A real sensor's certificate is signed by Abbott, and it accepts only app certificates
Abbott signed. libre3sim has its own trust root instead, made up on first run and kept in
`libre3sim-keys.json`: it signs every emulated sensor's certificate and the app credential
libre3sim hands out. None of it is Abbott key material. An app pairs with an emulated
sensor only when it trusts that root, which LibreLoop does in Simulator builds only:

- `GET /app-credentials` is the credential file for `AppIdentities.install(json:)`.
- `GET /patch-signing-key` is the key to add to `PairingFlow`'s `sensorCertSigningKeys`.
- `POST /nfc` stands in for the sensor's NFC tag, which the Simulator does not have.

## Run it for Loop in the iOS Simulator

```bash
go build -o libre3sim .
./libre3sim -fresh                 # a sensor still in its applicator
simulator/build-impossible.sh      # prints the path of libImpossiBLE.dylib

SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=/path/to/libImpossiBLE.dylib \
    xcrun simctl launch <simulator> <Loop's bundle id>
```

Then add a FreeStyle Libre 3 in Loop and scan: the scan goes to libre3sim, which activates
the sensor, and the app finds it over Bluetooth by its advertised address. The sensor warms
up for an hour (`-warmup 5m` for less; the sensor reports warmup in 5-minute steps).
`-age 3h` starts a sensor that is already being worn: pairing it is a switch-receiver scan,
the first app to scan it claims it, and readings flow at once.

From Xcode, set `DYLD_INSERT_LIBRARIES` under the scheme's Run → Arguments → Environment
Variables (a local change; don't commit it). Set `LIBRE3SIM_URL` there too if libre3sim's API
is not on `127.0.0.1:8080`.

ImpossiBLE's menu bar app serves the same socket, so quit it first, or give libre3sim a
socket of its own: `build-impossible.sh` adds a patch that reads the path from
`IMPOSSIBLE_SOCKET`, so each simulator can reach its own provider.

```bash
./libre3sim -fresh -socket /tmp/impossible-libre3.sock -api 127.0.0.1:8090
SIMCTL_CHILD_DYLD_INSERT_LIBRARIES=/path/to/libImpossiBLE.dylib \
SIMCTL_CHILD_IMPOSSIBLE_SOCKET=/tmp/impossible-libre3.sock \
SIMCTL_CHILD_LIBRE3SIM_URL=http://127.0.0.1:8090 \
    xcrun simctl launch <simulator> <Loop's bundle id>
```

To run beside g7sim or the pod simulator in one app, give each its own `-socket` and join
them with g7sim's `impossible-mux`; `-simulator`, `-relaunch` and `-suspend` work as in
g7sim (see its README).

A connect to a peripheral that is not the current sensor (one from before `/new`) is held,
never answered, as iOS holds a connect to a sensor that is out of range.

## Control API (`-api`, default `:8080`)

Open `http://localhost:8080/` for a control panel, or:

```bash
curl -s localhost:8080/status
curl -s -XPOST localhost:8080/glucose -d '{"kind":"fixed","value":65}'
curl -s -XPOST localhost:8080/glucose -d '{"kind":"sine","value":120,"amplitude":40,"periodMinutes":240}'
curl -s -XPOST localhost:8080/glucose -d '{"kind":"walk","value":140}'
curl -s -XPOST localhost:8080/disconnect                      # the link drops; the app reconnects
curl -s -XPOST localhost:8080/out-of-range -d '{"minutes":20}' # unreachable for 20 min, then backfill
curl -s -XPOST localhost:8080/terminate -d '{"errorData":8}'   # 6 terminated, 8 replace sensor
curl -s -XPOST localhost:8080/new -d '{"wearDays":15,"warmupMinutes":60,"ageMinutes":0}'
```

`/new` replaces the sensor and restarts libre3sim with it; the new sensor has its own
serial and BLE address.

## Credits and license

- The protocol is as implemented in RoundWhiteDiscKit, whose authors worked it out from
  captures; its tests' captured frames check libre3sim's encoder.
- The iOS Simulator transport, state restoration and suspension are g7sim's, and serve
  Mickey Lauer's [ImpossiBLE](https://github.com/mickeyl/ImpossiBLE) (MIT), which libre3sim
  does not vendor.

libre3sim is MIT licensed (see `LICENSE`).
