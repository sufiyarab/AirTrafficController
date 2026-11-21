"""How the simulator and the ground receivers reach the gateway.

TRANSPORT=grpc (default): service Gateway in proto/atc.proto, one unary call
per tick, to GATEWAY_GRPC (host:port, default gateway:9090).
TRANSPORT=http: the original JSON endpoints under GATEWAY (http://gateway:8080).

With MTLS=on (the default) the gRPC channel uses mutual TLS: this service
presents CERT_DIR/<CERT_NAME>.crt (CERT_NAME is "simulator", or the receiver's
RECEIVER_ID) and checks that the gateway's certificate is signed by the same
CA and names "gateway". The HTTP endpoints are closed while mTLS is on.

The generated stubs (atc_pb2.py, atc_pb2_grpc.py) are committed next to this
file; regenerate them after changing proto/atc.proto (see README).
"""
import json
import os
import urllib.request

TRANSPORT = os.environ.get("TRANSPORT", "grpc").strip().lower()
GATEWAY = os.environ.get("GATEWAY", "http://gateway:8080")
GATEWAY_GRPC = os.environ.get("GATEWAY_GRPC", "gateway:9090")
MTLS = os.environ.get("MTLS", "on").strip().lower() in ("", "on", "true", "1")
CERT_DIR = os.environ.get("CERT_DIR", "/certs")


class GatewayError(OSError):
    """The gateway did not take the batch. An OSError, so callers that
    already catch network errors (URLError is one too) need no change."""


class GatewayClient:
    def __init__(self, cert_name):
        if TRANSPORT not in ("grpc", "http"):
            raise SystemExit(f"TRANSPORT must be grpc or http, not {TRANSPORT!r}")
        if TRANSPORT == "http" and MTLS:
            raise SystemExit("TRANSPORT=http needs MTLS=off: with mutual TLS on, the gateway only takes input over gRPC")
        self.mtls = TRANSPORT == "grpc" and MTLS
        self.stub = None
        if TRANSPORT == "grpc":
            import grpc  # only needed for gRPC, so HTTP mode runs without it
            import atc_pb2
            import atc_pb2_grpc

            self.grpc, self.pb = grpc, atc_pb2
            # Reconnect within a second after the gateway restarts (gRPC's
            # default backoff grows to two minutes).
            options = [("grpc.initial_reconnect_backoff_ms", 100), ("grpc.max_reconnect_backoff_ms", 1000)]
            if self.mtls:
                def read(name):
                    with open(os.path.join(CERT_DIR, name), "rb") as f:
                        return f.read()

                creds = grpc.ssl_channel_credentials(
                    root_certificates=read("ca.crt"),
                    private_key=read(cert_name + ".key"),
                    certificate_chain=read(cert_name + ".crt"),
                )
                # Verify the server's certificate is the gateway's, whatever
                # address GATEWAY_GRPC uses to reach it.
                options.append(("grpc.ssl_target_name_override", "gateway"))
                channel = grpc.secure_channel(GATEWAY_GRPC, creds, options=options)
            else:
                channel = grpc.insecure_channel(GATEWAY_GRPC, options=options)
            self.stub = atc_pb2_grpc.GatewayStub(channel)

    def describe(self):
        if not self.stub:
            return f"HTTP {GATEWAY}"
        return f"gRPC {GATEWAY_GRPC}" + (" with mutual TLS" if self.mtls else "")

    def positions(self, claims, timeout=2):
        """Send one tick of claimed positions: dicts with callsign, seq, x, y, alt, heading, speed.

        Returns the gateway's reply: recent committed clearances of the
        aircraft in the batch, oldest first, as dicts with callsign,
        clearance_id and instruction (see apply_instructions in sim.py)."""
        if not self.stub:
            reply = self._post("/api/positions", claims, timeout)
            return (reply or {}).get("instructions") or []
        batch = self.pb.PositionBatch(positions=[
            self.pb.Position(
                callsign=c["callsign"], seq=int(c.get("seq", 0)), x_nm=c["x"], y_nm=c["y"],
                alt_ft=c["alt"], heading_deg=c["heading"], speed_kt=c["speed"],
            )
            for c in claims
        ])
        reply = self._call(self.stub.ReportPositions, batch, timeout)
        return [{"callsign": i.callsign, "clearance_id": i.clearance_id, "instruction": i.instruction}
                for i in reply.instructions]

    def observations(self, receiver_id, observations, timeout=2):
        """Send one receiver's ranges for a tick; an empty list is a heartbeat."""
        if not self.stub:
            return self._post("/api/observations", {"receiver_id": receiver_id, "observations": observations}, timeout)
        batch = self.pb.ObservationBatch(receiver_id=receiver_id, observations=[
            self.pb.Observation(callsign=o["callsign"], seq=int(o["seq"]), range_nm=o["range_nm"]) for o in observations
        ])
        self._call(self.stub.ReportObservations, batch, timeout)

    def _call(self, rpc, msg, timeout):
        try:
            return rpc(msg, timeout=timeout)
        except self.grpc.RpcError as error:
            raise GatewayError(f"{error.code().name}: {error.details()}") from None

    @staticmethod
    def _post(path, payload, timeout):
        req = urllib.request.Request(
            GATEWAY + path, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"}
        )
        body = urllib.request.urlopen(req, timeout=timeout).read()
        try:
            return json.loads(body) if body else None
        except ValueError:
            return None
