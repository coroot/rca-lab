FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt grpcio-tools==1.66.2
RUN opentelemetry-bootstrap -a install

COPY . .

ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi

RUN python -m grpc_tools.protoc \
    -I proto \
    --python_out=. \
    --grpc_python_out=. \
    proto/recommendation.proto

EXPOSE 8080

CMD ["opentelemetry-instrument", "uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8080", "--workers", "4"]
