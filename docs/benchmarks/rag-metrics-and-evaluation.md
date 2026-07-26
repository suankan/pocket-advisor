# 📋 Production RAG Audit Checklist

**Status:** Draft candidate — **not implemented**; roadmap item 3 in
`docs/roadmap.md`. A generic industry checklist kept as reference material
for a future generation-quality evaluation pass. Nothing below describes
current code; the implemented retrieval-side accuracy harness is
`docs/benchmarks/accuracy-testing.md`.

Use this checklist to grade your current RAG implementation and identify immediate engineering gaps.

## Phase 1: Foundation & Pipeline Isolation

* [ ] **Beyond the "Vibe Check":** Do you have an automated, quantitative evaluation pipeline running, or are you still relying on manual prompt testing ("vibe checking") to verify changes?
* [ ] **Isolated Component Testing:** Can you evaluate your **Retrieval** mechanism completely separate from your **Generation** mechanism? *(Crucial for pinpointing whether a bad answer is caused by a bad search or an LLM hallucination).*

## Phase 2: Retrieval Evaluation

*Are you measuring search quality using these 5 core metrics?*

* [ ] **Precision:** Are you tracking the percentage of retrieved chunks that are *actually* relevant to ensure you aren't wasting the context window?
* [ ] **Recall:** Are you measuring if the system successfully fetches *all* necessary information required to answer the query?
* [ ] **Hit Rate:** Do you know the percentage of times at least one correct document lands in your top results?
* [ ] **Mean Reciprocal Rank (MRR):** If your users rely on the very first response (like a standard search engine), are you scoring how close the best chunk is to the top slot?
* [ ] **NDCG:** If you display long lists or multi-document summaries, are you penalizing the system when highly relevant documents are buried at the bottom?

## Phase 3: Generation Evaluation

*Are you measuring the LLM's output using these 3 core metrics?*

* [ ] **Faithfulness:** Are you explicitly checking the output against the retrieved source chunks to actively catch and prevent hallucinations?
* [ ] **Answer Relevancy:** Are you tracking whether the LLM actually addresses the user's explicit question, or if it is providing polite but unhelpful filler?
* [ ] **Answer Correctness:** Do you compare the final output against a verified "ground truth" reference answer?

## Phase 4: Dataset & Judge Architecture

* [ ] **Synthetic to Real Pipeline:** Did you generate your test queries using a powerful LLM against your own index, and were they manually reviewed/refined to mimic actual human behavior?
* [ ] **LLM-as-a-Judge Architecture:** If you use an LLM to grade your system, have you implemented these 5 anti-bias guardrails?
* [ ] **Pairwise Comparison:** Does the judge compare two candidate responses side-by-side rather than grading one in a vacuum?
* [ ] **Order Randomization:** Do you swap the presentation order of the responses to eliminate position bias?
* [ ] **Tie Allowances:** Is the judge explicitly allowed to declare two answers equally good?
* [ ] **Forced Chain of Thought:** Does the judge script out its step-by-step reasoning *before* it assigns a final score?
* [ ] **Length Normalization:** Are test responses kept at a similar length so the judge isn't fooled by raw verbosity?



## Phase 5: Production Analytics & Tooling

* [ ] **Query Logging:** Are you capturing and storing production user queries in a database?
* [ ] **Topic Clustering:** Have you analyzed those logged queries to identify the high-frequency topics (the 80/20 rule) so you can focus your engineering efforts on what users actually ask?
* [ ] **Leveraging Tooling:** Are you using dedicated evaluation frameworks (like `ragas`) instead of custom-coding your metric math from scratch?
* [ ] **Human-in-the-Loop:** Do you have a secondary tier for human evaluation, such as running live A/B tests or user acceptance testing, to check for real-world fluency and utility?
