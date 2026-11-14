FROM python:3.12-slim
WORKDIR /app
COPY pyproject.toml ./
COPY esketit_agent ./esketit_agent
RUN pip install --no-cache-dir . && useradd --create-home agent
USER agent
EXPOSE 8312
CMD ["uvicorn", "esketit_agent.api:application", "--factory", "--host", "0.0.0.0", "--port", "8312", "--no-access-log"]
