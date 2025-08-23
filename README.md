# Jupiterp API

Welcome to the Jupiterp API.

## Deploy

To deploy this API on GCP:

```
PROJECT_ID=(insert GCP project ID here)
RUNTIME_SA="run-sa@${PROJECT_ID}.iam.gserviceaccount.com"
gcloud run deploy go-api \
  --source=. \
  --region="${REGION}" \
  --service-account="${RUNTIME_SA}" \
  --update-secrets="DATABASE_URL=DATABASE_URL:latest,DATABASE_KEY=DATABASE_KEY:latest"
```