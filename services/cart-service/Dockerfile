FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
RUN opentelemetry-bootstrap -a install
COPY . .
ARG VARIANT=""
RUN if [ -n "$VARIANT" ]; then cp -rf variants/$VARIANT/src/. ./; fi
EXPOSE 8080
CMD ["opentelemetry-instrument", "gunicorn", "--bind", "0.0.0.0:8080", "--workers", "4", "--threads", "4", "--timeout", "30", "--worker-tmp-dir", "/dev/shm", "app:app"]
