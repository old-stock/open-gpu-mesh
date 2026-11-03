# Multi-arch by construction: no RUN steps, so it builds natively and instantly on x86 and ARM.
FROM python:3.12-slim
COPY app.py /app/app.py
ENV PYTHONUNBUFFERED=1 PORT=8080
EXPOSE 8080
CMD ["python3", "/app/app.py"]
