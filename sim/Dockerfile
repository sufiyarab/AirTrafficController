
# Simulator and receivers share this image.
FROM python:3.12-slim

WORKDIR /app

# gRPC to the gateway (gateway_client.py). Pinned to the versions the
# committed stubs (atc_pb2*.py) were generated with.
RUN pip install --no-cache-dir grpcio==1.85.3 protobuf==7.36.1

# Copy Python source files.
COPY *.py ./

# Include predefined aircraft scenarios.
COPY scenarios/ ./scenarios/

ENV PYTHONUNBUFFERED=1

CMD ["python", "sim.py"]
