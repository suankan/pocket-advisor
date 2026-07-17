# Workspace Parsing Design

##  Stage 1 Discovery: Find Ingestion Candidates

Walk through all collections which are a part of workspaces which are "active: true" as per the workspace config.

The goal of this stage is to assess the volume and identify the items which need to be ingested.

Currently we basically support TWO types of documents: email files and pdf files.

At this stage we work with the Source of Truth Corpora data.

We record each item and their properties in DB, e.g. sha256, mark their document_type: ["email", "pdf"], date of the operation, mark them as candidates for Ingestion, record their
size.

This stage walk is idempotent.

This stage is also agnostic of the particular document-specifics.

The goal of this Stage is to obtain a working set for ingestion.

## Stage 2 Parse Emails.

For each email from Stage 1:

A folder is created under `workspaces/.state/cache/<collection_id>/<orig_email_basename>`

Inside the <orig_email_basename> folder:
    Files:
    - email_body.txt
    Folders:
    - attachments:
        - pdf-original:
        - pdf-to-text:
        - images:
        - zip-archives:
        - other:

Each email it teared in pieces and the pieces are put into the above folder structure.
AND
Emails metadata (headers, senders, recitients etc) is recorded in DB with the link to cache file locations and sha256 sums and links to the "parent" email objects.

Special case 1:

IF an email contains other email files attached:
  THEN for each of these attache email files we do Stage 1.
  AND this Stage 1 - Stage 2 process repeats
  UNTIL there are no more attached email files.
  All emails including the attached email files should receive their own folder `workspaces/.state/cache/<collection_id>/<orig_email_basename>`. No inner folders.

Special case 2:
IF an email contains an attached zip archive:
  THEN the archive is unpacked AND walked as in Stage 1
  UNTIL there are no more attached email files in attached archives.
  All emails including the attached email files should receive their own folder `workspaces/.state/cache/<collection_id>/<orig_email_basename>`. No inner folders.
  If zip contains just other files like pdfs or images - they are placed into that "parent" email folder as per the above structure.


## Stage 3

### 3.1 Find and prepare PDFs

After Stage 1 <-> Stage 2 recursive iterations ALL the PDF files belonging to the emails must be in cache locations like: `workspaces/.state/cache/<collection_id>/<orig_email_basename>/attachments/pdf/`
ALSO
There are still original PDFs in Coprora. For them we just copy them into `workspaces/.state/cache/<collection_id>/<original_pdf_filename>`

### 3.2 pdf-to-text

After 3.1 is done we have a full set of PDFs which we need to convert into plain text.
We do that via the pipeline:

#### 3.2.1
"ocrmypdf --redo-ocr --deskew --clean --clean-final --jobs <max_reasonable_cpu> <workspace_cache_pdf_location> <workspace_cache_pdf_location>-ocrmypdf.pdf"

WHERE <workspace_cache_pdf_location> is one of the:
- type: from-email-attachments
  location: workspaces/.state/cache/<collection_id>/<orig_email_basename>/attachments/pdf-original/
- type: from-corpora-native
  workspaces/.state/cache/<collection_id>/

#### 3.2.2
"pdftotext -layout <workspace_cache_pdf_location>-ocrmypdf.pdf <workspace_cache_pdf_location>.txt"

As a result of Stage 3 we should get the set of the plain text files:

<workspace_cache_pdf_location>.txt

Which splits into two locations:

- type: from-email-attachments
  location: workspaces/.state/cache/<collection_id>/<orig_email_basename>/attachments/pdf-to-text/*.txt
- type: from-corpora-native
  workspaces/.state/cache/<collection_id>/pdf-to-text/*.txt

### Stage 4 Embedding the plain text artifacts

- type: from-email-body
  location: workspaces/.state/cache/<collection_id>/<orig_email_basename>/email_body.txt
- type: from-email-attachments
  location: workspaces/.state/cache/<collection_id>/<orig_email_basename>/attachments/pdf-to-text/*.txt
- type: from-corpora-native
  location: workspaces/.state/cache/<collection_id>/pdf-to-text/*.txt
