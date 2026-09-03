# Example Tool Ideas for DeskBox

These 10 tool designs show the range of what DeskBox can proxy. Each is a realistic example that teaches something different about the tool contract: network calls, file handling, structured input, long-running jobs, and credential passing. None are implemented, just specified.

---

## 1. stripe-create-charge

Push a payment charge to Stripe, demonstrating network access with API credentials in the input.

**Language:** bash

**Purpose:** Shows how tools handle external API calls with sensitive credentials. The contract demonstrates that credentials travel as input data, not from the host environment. Contrasts with a network-free tool.

**tcs.yaml:**
```yaml
name: stripe-create-charge
summary: Create a payment charge in Stripe
input:
  type: object
  required:
    - stripe_api_key
    - amount_cents
    - currency
    - card_token
  properties:
    stripe_api_key:
      type: string
      description: Stripe secret API key
    amount_cents:
      type: integer
      minimum: 1
    currency:
      type: string
      enum: [usd, eur, gbp]
    card_token:
      type: string
      description: Stripe tokenized card ID
    metadata:
      type: object
      additionalProperties:
        type: string
output:
  schema:
    type: object
    properties:
      charge_id:
        type: string
      amount:
        type: integer
      status:
        type: string
        enum: [succeeded, failed, pending]
      timestamp:
        type: string
allowed_side_effects:
  files: []
  network: true
sandbox:
  in: []
  out: []
execution:
  mode: direct
  timeout_ms: 10000
  max_retries: 2
```

**Implementation outline:** Reads stripe_api_key, amount_cents, currency, card_token from input JSON on stdin. Constructs curl call to Stripe charges endpoint with Bearer auth header. Parses JSON response. Returns charge_id, amount, status, timestamp as single JSON object to stdout. On network error, retries up to 2 times.

---

## 2. slack-fetch-messages

Pull recent messages from a Slack channel, showing external data retrieval and pagination.

**Language:** python3

**Purpose:** Demonstrates pulling data from a real service over network. Input includes pagination token to show how tools handle stateful iteration across API calls. Output is an array of structured objects.

**tcs.yaml:**
```yaml
name: slack-fetch-messages
summary: Fetch recent messages from a Slack channel
input:
  type: object
  required:
    - slack_api_token
    - channel_id
    - limit
  properties:
    slack_api_token:
      type: string
      description: Slack bot OAuth token
    channel_id:
      type: string
    limit:
      type: integer
      minimum: 1
      maximum: 100
    cursor:
      type: string
      description: Pagination cursor from prior call
output:
  schema:
    type: object
    properties:
      messages:
        type: array
        items:
          type: object
          properties:
            ts:
              type: string
            user:
              type: string
            text:
              type: string
            reactions:
              type: array
              items:
                type: string
      next_cursor:
        type: string
        description: Pass to next call for pagination
      has_more:
        type: boolean
allowed_side_effects:
  files: []
  network: true
sandbox:
  in: []
  out: []
execution:
  mode: direct
  timeout_ms: 15000
  max_retries: 3
```

**Implementation outline:** Parses input JSON. Calls Slack conversations.history API with Bearer token in header. Handles 429 rate limits with exponential backoff across retries. Extracts ts, user, text, reactions from each message. Returns paginated response with next_cursor for continuation. If API returns error, propagates it to stderr with non-zero exit.

---

## 3. generate-pdf-report

Generate a multi-file PDF report with cover page and appendices, demonstrating multiple file side effects.

**Language:** node/javascript

**Purpose:** Shows a tool that writes more than one file to the output sandbox. Input specifies what to include. Output declares multiple files created. This teaches that tools are not limited to single-document output.

**tcs.yaml:**
```yaml
name: generate-pdf-report
summary: Generate a multi-page PDF report with cover and data tables
input:
  type: object
  required:
    - title
    - sections
  properties:
    title:
      type: string
    sections:
      type: array
      minItems: 1
      items:
        type: object
        required:
          - name
          - content
        properties:
          name:
            type: string
          content:
            type: string
          include_page_break:
            type: boolean
output:
  schema:
    type: object
    properties:
      report_file:
        type: string
        description: Path to main PDF
      manifest:
        type: object
        properties:
          total_pages:
            type: integer
          files_created:
            type: array
            items:
              type: string
allowed_side_effects:
  files:
    - /deskbox/out/report.pdf
    - /deskbox/out/metadata.json
  network: false
sandbox:
  in: []
  out:
    - /deskbox/out
execution:
  mode: direct
  timeout_ms: 30000
  max_retries: 0
```

**Implementation outline:** Reads title and sections array from input. Uses a PDF library to construct document: adds cover page with title, iterates sections and adds content with optional page breaks. Writes main PDF to /deskbox/out/report.pdf. Creates metadata.json alongside with page count and manifest. Returns JSON pointing to report_file path and nested manifest with total_pages and files_created array. No network access. Non-zero exit if PDF generation fails.

---

## 4. validate-email

Validate email syntax with no side effects, designed for direct fast execution.

**Language:** bash

**Purpose:** A simple, synchronous tool for immediate feedback. No retries needed, no file I/O, just validation logic. Demonstrates execution.mode: direct and tight timeout for quick operations.

**tcs.yaml:**
```yaml
name: validate-email
summary: Validate email address format and check MX records locally
input:
  type: object
  required:
    - email
  properties:
    email:
      type: string
      minLength: 5
      maxLength: 254
output:
  schema:
    type: object
    properties:
      valid:
        type: boolean
      reason:
        type: string
      format_correct:
        type: boolean
      has_common_typo:
        type: boolean
allowed_side_effects:
  files: []
  network: false
sandbox:
  in: []
  out: []
execution:
  mode: direct
  timeout_ms: 1000
  max_retries: 0
```

**Implementation outline:** Reads email from input. Applies regex to check format against simplified RFC 5322 rules. Checks domain against list of common typos (gmai.com, yahooo.com, etc). Returns JSON with valid bool, reason string, format_correct bool, has_common_typo bool. All checks run locally, no network. Returns result in under 100ms. Exit zero on success, non-zero on invalid input.

---

## 5. train-ml-model

Train a machine learning model with long runtime and automatic retries on failure.

**Language:** python3

**Purpose:** Shows a queued tool that runs long and may benefit from retries. Execution mode is queued with max_retries > 0 and long timeout. Demonstrates how DeskBox handles background jobs that may fail transiently and need re-running.

**tcs.yaml:**
```yaml
name: train-ml-model
summary: Train a scikit-learn model on CSV data
input:
  type: object
  required:
    - model_type
    - training_data_path
    - hyperparameters
  properties:
    model_type:
      type: string
      enum: [random_forest, gradient_boost, svm]
    training_data_path:
      type: string
    hyperparameters:
      type: object
      properties:
        n_estimators:
          type: integer
          minimum: 10
          maximum: 1000
        learning_rate:
          type: number
          minimum: 0.001
          maximum: 1.0
output:
  schema:
    type: object
    properties:
      model_id:
        type: string
      accuracy:
        type: number
      precision:
        type: number
      recall:
        type: number
      training_time_seconds:
        type: integer
allowed_side_effects:
  files:
    - /deskbox/out
  network: false
sandbox:
  in:
    - /deskbox/in/training_data.csv
  out:
    - /deskbox/out
execution:
  mode: queued
  timeout_ms: 600000
  max_retries: 3
```

**Implementation outline:** Reads model_type and hyperparameters from input JSON. Loads CSV training data from /deskbox/in/training_data.csv (bound read-only). Instantiates scikit-learn model with given hyperparameters. Trains on data, measuring accuracy, precision, recall. Saves serialized model to /deskbox/out/model.pkl with unique model_id (UUID). Returns JSON with model_id, accuracy, precision, recall, training_time_seconds. If training fails (OOM, bad hyperparameters), exits non-zero; DeskBox will retry up to 3 times with exponential backoff.

---

## 6. github-create-issue

Create an issue on GitHub with nested object input, showing structured complexity.

**Language:** go

**Purpose:** Demonstrates a tool with nested/complex input schema (labels array, nested assignee object, metadata object). Shows how the JSON schema subset supports real-world API requirements. Also demonstrates network access to a different service than the prior examples.

**tcs.yaml:**
```yaml
name: github-create-issue
summary: Create an issue in a GitHub repository
input:
  type: object
  required:
    - github_token
    - owner
    - repo
    - title
  properties:
    github_token:
      type: string
    owner:
      type: string
    repo:
      type: string
    title:
      type: string
      minLength: 1
      maxLength: 256
    body:
      type: string
    labels:
      type: array
      items:
        type: string
      maxItems: 10
    assignees:
      type: array
      items:
        type: string
      maxItems: 5
    milestone:
      type: integer
    metadata:
      type: object
      additionalProperties:
        type: string
output:
  schema:
    type: object
    properties:
      issue_number:
        type: integer
      issue_url:
        type: string
      created_at:
        type: string
allowed_side_effects:
  files: []
  network: true
sandbox:
  in: []
  out: []
execution:
  mode: direct
  timeout_ms: 15000
  max_retries: 2
```

**Implementation outline:** Parses input JSON and extracts github_token, owner, repo, title, body, labels, assignees. Constructs GitHub REST API request to POST /repos/{owner}/{repo}/issues with nested arrays and objects in the payload. Includes Bearer token in Authorization header. Parses response JSON and extracts issue_number, issue_url, created_at. Returns as JSON to stdout. On 401/403 errors, exits non-zero. On rate limit, retries once after delay.

---

## 7. scan-file-for-pii

Scan a file for personally identifiable information, showing network:false with complex file side effects.

**Language:** bash

**Purpose:** Demonstrates a tool that has no network access but does complex file I/O. Input is a file path, output is a report file plus structured metadata. Shows the contrast with network tools and how file side effects work without external services.

**tcs.yaml:**
```yaml
name: scan-file-for-pii
summary: Scan text file for PII patterns, output findings to report
input:
  type: object
  required:
    - file_name
  properties:
    file_name:
      type: string
    scan_patterns:
      type: array
      items:
        type: string
        enum: [ssn, credit_card, email, phone, ip_address]
      minItems: 1
      maxItems: 6
output:
  schema:
    type: object
    properties:
      findings_count:
        type: integer
      report_file:
        type: string
      patterns_detected:
        type: array
        items:
          type: string
      risk_level:
        type: string
        enum: [none, low, medium, high]
allowed_side_effects:
  files:
    - /deskbox/out
  network: false
sandbox:
  in:
    - /deskbox/in
  out:
    - /deskbox/out
execution:
  mode: direct
  timeout_ms: 30000
  max_retries: 0
```

**Implementation outline:** Reads file_name and scan_patterns from input. Reads file from /deskbox/in/{file_name}. Applies regex patterns for each requested scan type (SSN: 9 digits, credit card: Luhn check, email: standard pattern, phone: 10-digit US or international, IP address: dotted quad). Counts matches. Writes detailed CSV report to /deskbox/out/pii-scan-report.csv with line number, pattern type, matched text. Returns JSON with findings_count, report_file path, patterns_detected array, risk_level (none=0, low=1-5, medium=6-20, high=20+). No network calls. Exit non-zero if input file does not exist.

---

## 8. compress-images

Batch compress images, demonstrating array processing and multiple file I/O.

**Language:** bash with ImageMagick

**Purpose:** Shows a tool that takes an array of input files and produces multiple output files. Input is an array of image names and compression options. Output is metadata about each compressed image. Teaches batch processing without requiring all files in one blob.

**tcs.yaml:**
```yaml
name: compress-images
summary: Compress images in batch using ImageMagick
input:
  type: object
  required:
    - image_files
    - quality
  properties:
    image_files:
      type: array
      minItems: 1
      maxItems: 100
      items:
        type: string
    quality:
      type: integer
      minimum: 1
      maximum: 100
    format:
      type: string
      enum: [jpeg, webp, png]
      default: jpeg
output:
  schema:
    type: object
    properties:
      compressed_images:
        type: array
        items:
          type: object
          properties:
            original_name:
              type: string
            output_name:
              type: string
            original_size_bytes:
              type: integer
            compressed_size_bytes:
              type: integer
            compression_ratio:
              type: number
      total_bytes_saved:
        type: integer
allowed_side_effects:
  files:
    - /deskbox/out
  network: false
sandbox:
  in:
    - /deskbox/in
  out:
    - /deskbox/out
execution:
  mode: direct
  timeout_ms: 60000
  max_retries: 0
```

**Implementation outline:** Reads image_files array and quality setting from input. Iterates each image file name in the array. For each, reads from /deskbox/in/{name}, uses convert (ImageMagick) to compress to specified quality and format, writes to /deskbox/out/{name}-compressed.{ext}. Records original size, new size, calculates ratio. Builds JSON array of results with original_name, output_name, original_size_bytes, compressed_size_bytes, compression_ratio. Sums bytes saved. Returns JSON to stdout with array of results and total_bytes_saved. If any image fails to convert, logs error to stderr but continues processing remaining images.

---

## 9. db-backup-status

Check the status of a database backup job with queued execution and retries.

**Language:** python3

**Purpose:** Simulates polling a long-running backend job (like a database backup). Uses queued execution mode with retries and a longer timeout. Output schema includes status enum. Teaches how tools can wrap polling logic around external operations.

**tcs.yaml:**
```yaml
name: db-backup-status
summary: Initiate database backup and poll for completion
input:
  type: object
  required:
    - database_connection_string
    - backup_id
  properties:
    database_connection_string:
      type: string
    backup_id:
      type: string
    poll_timeout_seconds:
      type: integer
      minimum: 60
      maximum: 3600
      default: 600
output:
  schema:
    type: object
    properties:
      backup_id:
        type: string
      status:
        type: string
        enum: [pending, running, completed, failed]
      percent_complete:
        type: integer
      size_bytes:
        type: integer
      duration_seconds:
        type: integer
      error_message:
        type: string
allowed_side_effects:
  files: []
  network: true
sandbox:
  in: []
  out: []
execution:
  mode: queued
  timeout_ms: 1200000
  max_retries: 5
```

**Implementation outline:** Parses database_connection_string and backup_id from input. Opens connection to database (PostgreSQL, MySQL, etc). Queries backup status table for the given backup_id. Polls status every 10 seconds up to poll_timeout_seconds total. Records percent_complete from the database, size_bytes written so far, elapsed duration. On completion or failure, returns final JSON with backup_id, status (pending/running/completed/failed), percent_complete, size_bytes, duration_seconds, and error_message if failed. If status check fails, exits non-zero; DeskBox will retry up to 5 times. Retries are useful if database is temporarily unavailable.

---

## 10. aws-s3-upload

Upload a file to Amazon S3, demonstrating real-world credential and network requirements.

**Language:** node/javascript

**Purpose:** Combines credential passing, network access, and file I/O side effects. Demonstrates a complete flow: read file from input sandbox, upload to cloud, track the remote operation. Shows how a tool bridges local sandbox and external service.

**tcs.yaml:**
```yaml
name: aws-s3-upload
summary: Upload file to Amazon S3 bucket
input:
  type: object
  required:
    - aws_access_key
    - aws_secret_key
    - bucket_name
    - local_file_name
    - s3_key
  properties:
    aws_access_key:
      type: string
    aws_secret_key:
      type: string
    bucket_name:
      type: string
    local_file_name:
      type: string
    s3_key:
      type: string
      description: Path in bucket
    region:
      type: string
      enum: [us-east-1, us-west-2, eu-west-1, ap-southeast-1]
      default: us-east-1
    acl:
      type: string
      enum: [private, public-read]
      default: private
    metadata:
      type: object
      additionalProperties:
        type: string
output:
  schema:
    type: object
    properties:
      s3_url:
        type: string
      etag:
        type: string
      size_bytes:
        type: integer
      upload_duration_seconds:
        type: number
allowed_side_effects:
  files: []
  network: true
sandbox:
  in:
    - /deskbox/in
  out: []
execution:
  mode: direct
  timeout_ms: 120000
  max_retries: 2
```

**Implementation outline:** Parses aws_access_key, aws_secret_key, bucket_name, local_file_name, s3_key, region, acl from input JSON. Reads file from /deskbox/in/{local_file_name}. Constructs AWS S3 API request (or uses SDK like aws-sdk-js) with SigV4 authentication using credentials. Uploads with specified ACL and metadata headers. Polls for completion if multipart upload is used. Captures response with ETag, size confirmation, upload duration. Returns JSON to stdout with s3_url (https://bucket.s3.region.amazonaws.com/s3_key), etag, size_bytes, upload_duration_seconds. On auth failure, exits non-zero immediately. On network timeout, retries up to 2 times.

---

## Summary

These 10 examples cover the key design patterns DeskBox enforces:

- **Network handling**: tools 1, 2, 6, 9, 10 use network:true with credentials embedded in input; tools 3, 4, 7, 8 run locally with network:false.
- **Execution modes**: tools 4, 5, 8, 10 use direct (fast, synchronous); tools 5, 9 use queued (long-running, retriable).
- **File I/O**: tools 3, 5, 7, 8 write to sandbox.out; tools 2, 5, 7, 8, 9 read from sandbox.in; most tools avoid files entirely.
- **Input complexity**: tools 1, 2, 6, 8, 9, 10 use nested objects or arrays; tools 4, 7 keep input simple.
- **Languages**: bash (1, 4, 7, 8), python3 (2, 5, 9), node (3, 10), go (6) show that the tool contract is language-agnostic.

Each tool teaches something specific about the contract while remaining realistic enough to guide real tool design.
