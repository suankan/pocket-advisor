"""The five ingestion services and the runtime that hosts them.

`docs/ingestion/document-flow-services.md`. One hub that decides and settles,
four workers that do the work, and a "document" on every wire between them.

    documents.py      the wire contract every service speaks
    base.py           the Service interface: queue, pool, answer, lifecycle
    api.py            the REST door, the client, and the Lane
    state.py          the one thread allowed to mutate relational state
    management.py     the hub: walk · route · register · settle
    extraction.py     pure MIME extraction (composed by the emails service)
    registrar.py      the writer-thread half that turns a graph into rows
    emails.py         EmailsProcessingService
    pdftotext.py      PdfToTextService
    embedding.py      PlainTextEmbeddingService
    summarisation.py  SummarisationEmbeddingService
    orchestrator.py   composition and closure order for one run
"""
