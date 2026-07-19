"""Typed hot-stage performance telemetry (ingest-report schema v2).

One PerformanceTelemetry recorder is created by CLI orchestration before any
stage runs and injected through the PipelineContext. Stages update aggregate
counters and subphase timers while doing their existing work; the recorder
therefore survives a stage exception with the measurements captured so far.

Every hot-stage object carries an explicit measurement state so a zero never
ambiguously means disabled, unmeasured, unchanged, or failed:

- ``measured``       — the stage ran and captured all required telemetry
                       (including a legitimate unchanged run with zero work);
- ``not_applicable`` — the stage was deliberately disabled or out of scope;
- ``partial``        — the stage was entered but failed or was interrupted;
- ``not_run``        — orchestration never entered the stage.

The record is aggregate-only: counters, token totals, and timings. No field
may carry filenames, subjects, Message-IDs, or corpus text.
"""
import math
from dataclasses import asdict, dataclass, field, fields, replace
from typing import Any

from modules.summarization import SUMMARY_LENGTH_TIER_BOUNDS


MEASURED = "measured"
NOT_APPLICABLE = "not_applicable"
PARTIAL = "partial"
NOT_RUN = "not_run"
STATES = frozenset((MEASURED, NOT_APPLICABLE, PARTIAL, NOT_RUN))

HOT_STAGE_NAMES = ("summaries", "embed", "pdfs")

class TelemetryError(ValueError):
    """A performance record violates the locked schema-v2 contract."""


@dataclass(slots=True)
class LengthTier:
    """One deterministic summary-input length tier; the unbounded final
    tier uses upper_bound_tokens=None."""

    upper_bound_tokens: int | None
    threads: int = 0
    generation_calls: int = 0


@dataclass(slots=True)
class SummariesTimings:
    input_render: float = 0.0
    model_execution: float = 0.0
    publication: float = 0.0


@dataclass(slots=True)
class SummariesTelemetry:
    state: str = NOT_RUN
    eligible_threads: int = 0
    pending_threads: int = 0
    unchanged_threads: int = 0
    completed_threads: int = 0
    failed_threads: int = 0
    input_messages: int = 0
    input_segments: int = 0
    generation_calls: int = 0
    total_input_tokens: int = 0
    one_shot_threads: int = 0
    hierarchical_threads: int = 0
    overflow_reductions: int = 0
    length_tiers: list[LengthTier] = field(default_factory=list)
    timings_seconds: SummariesTimings = field(
        default_factory=SummariesTimings)

    def new_tiers(self) -> list[LengthTier]:
        """Install and return the fixed, deterministically ordered tiers."""
        self.length_tiers = [
            LengthTier(bound) for bound in SUMMARY_LENGTH_TIER_BOUNDS]
        self.length_tiers.append(LengthTier(None))
        return self.length_tiers

    def tier_for(self, input_tokens: int) -> LengthTier:
        for tier in self.length_tiers:
            if tier.upper_bound_tokens is None \
                    or input_tokens <= tier.upper_bound_tokens:
                return tier
        raise TelemetryError("summaries.length_tiers has no unbounded tier")


@dataclass(slots=True)
class EmbedQueueTelemetry:
    pending_entities: int = 0
    input_tokens: int = 0
    bucket_count: int = 0
    microbatch_count: int = 0
    padding_tokens: int = 0
    successful_entities: int = 0
    failed_entities: int = 0
    individual_fallbacks: int = 0
    bisection_fallbacks: int = 0


@dataclass(slots=True)
class EmbedQueues:
    leaf: EmbedQueueTelemetry = field(default_factory=EmbedQueueTelemetry)
    summary: EmbedQueueTelemetry = field(
        default_factory=EmbedQueueTelemetry)


@dataclass(slots=True)
class EmbedTimings:
    model_execution: float = 0.0
    cache_publication: float = 0.0
    matrix_assembly: float = 0.0


@dataclass(slots=True)
class EmbedTelemetry:
    state: str = NOT_RUN
    queues: EmbedQueues = field(default_factory=EmbedQueues)
    verified_cache_publications: int = 0
    timings_seconds: EmbedTimings = field(default_factory=EmbedTimings)


@dataclass(slots=True)
class PdfFanOut:
    copies: int = 0
    copy_on_write_clones: int = 0


@dataclass(slots=True)
class PdfResources:
    configured_worker_count: int = 0
    configured_per_child_jobs: int = 0
    configured_global_cpu_budget: int = 0
    observed_peak_workers: int = 0
    process_tree_peak_rss_bytes: int | None = None


@dataclass(slots=True)
class PdfsTimings:
    transform_wall: float = 0.0
    ocr_process_total: float = 0.0
    text_process_total: float = 0.0
    fan_out_publication: float = 0.0


@dataclass(slots=True)
class PdfsTelemetry:
    state: str = NOT_RUN
    occurrences_considered: int = 0
    pending_occurrences: int = 0
    unique_transforms: int = 0
    successful_transforms: int = 0
    failed_transforms: int = 0
    duplicate_reuses: int = 0
    direct_original_fallbacks: int = 0
    fan_out: PdfFanOut = field(default_factory=PdfFanOut)
    resources: PdfResources = field(default_factory=PdfResources)
    timings_seconds: PdfsTimings = field(default_factory=PdfsTimings)


@dataclass(slots=True)
class PerformanceTelemetry:
    """The complete typed run recorder for the three hot stages."""

    summaries: SummariesTelemetry = field(
        default_factory=SummariesTelemetry)
    embed: EmbedTelemetry = field(default_factory=EmbedTelemetry)
    pdfs: PdfsTelemetry = field(default_factory=PdfsTelemetry)

    def _stage(self, name: str):
        if name not in HOT_STAGE_NAMES:
            raise TelemetryError(f"unknown hot stage: {name!r}")
        return getattr(self, name)

    def mark_entered(self, name: str) -> None:
        stage = self._stage(name)
        if stage.state == NOT_RUN:
            stage.state = PARTIAL

    def mark_measured(self, name: str) -> None:
        """Seal a completed stage; a deliberate not_applicable gate
        recorded by the stage itself is preserved."""
        stage = self._stage(name)
        if stage.state == PARTIAL:
            stage.state = MEASURED

    def mark_not_applicable(self, name: str) -> None:
        """Record a deliberate gate: same shape, zero counters/timings."""
        fresh = type(self._stage(name))(state=NOT_APPLICABLE)
        setattr(self, name, fresh)

    def as_json_dict(self) -> dict[str, Any]:
        return asdict(self)

    def validate(self) -> None:
        _validate_summaries(self.summaries)
        _validate_embed(self.embed)
        _validate_pdfs(self.pdfs)


# -- strict validation -------------------------------------------------------


def _check_count(value: Any, where: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise TelemetryError(f"{where} must be an integer, got {value!r}")
    if value < 0:
        raise TelemetryError(f"{where} must be non-negative, got {value}")
    return value


def _check_timing(value: Any, where: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise TelemetryError(f"{where} must be a number, got {value!r}")
    number = float(value)
    if not math.isfinite(number) or number < 0:
        raise TelemetryError(
            f"{where} must be a finite non-negative number, got {value!r}")
    return number


def _check_state(value: Any, where: str) -> str:
    if value not in STATES:
        raise TelemetryError(f"{where} must be one of {sorted(STATES)},"
                             f" got {value!r}")
    return value


def _check_counts(obj, names: tuple[str, ...], where: str) -> None:
    for name in names:
        _check_count(getattr(obj, name), f"{where}.{name}")


def _check_timings(obj, where: str) -> None:
    for spec in fields(obj):
        _check_timing(getattr(obj, spec.name), f"{where}.{spec.name}")


def _is_zero(obj, reference) -> bool:
    return asdict(replace(obj, state=reference.state)) == asdict(reference)


def _require_zero(obj, where: str) -> None:
    reference = type(obj)(state=obj.state)
    if not _is_zero(obj, reference):
        raise TelemetryError(
            f"{where} is {obj.state} and must keep zero counters,"
            " zero timings, and empty tiers")


_SUMMARY_COUNTS = (
    "eligible_threads", "pending_threads", "unchanged_threads",
    "completed_threads", "failed_threads", "input_messages",
    "input_segments", "generation_calls", "total_input_tokens",
    "one_shot_threads", "hierarchical_threads", "overflow_reductions",
)


def _validate_tiers(tiers: list[LengthTier], where: str) -> None:
    previous: int | None = None
    for index, tier in enumerate(tiers):
        label = f"{where}[{index}]"
        bound = tier.upper_bound_tokens
        if bound is not None:
            _check_count(bound, f"{label}.upper_bound_tokens")
            if index != 0 and previous is None:
                raise TelemetryError(
                    f"{label}: only the final tier may be unbounded")
            if previous is not None and bound <= previous:
                raise TelemetryError(
                    f"{label}: tier bounds must strictly ascend")
        elif index != len(tiers) - 1:
            raise TelemetryError(
                f"{label}: the unbounded tier must be last")
        _check_count(tier.threads, f"{label}.threads")
        _check_count(tier.generation_calls, f"{label}.generation_calls")
        previous = bound


def _validate_summaries(obj: SummariesTelemetry) -> None:
    where = "performance.summaries"
    _check_state(obj.state, f"{where}.state")
    _check_counts(obj, _SUMMARY_COUNTS, where)
    _validate_tiers(obj.length_tiers, f"{where}.length_tiers")
    _check_timings(obj.timings_seconds, f"{where}.timings_seconds")
    if obj.state in (NOT_RUN, NOT_APPLICABLE):
        _require_zero(obj, where)
        return
    assigned = obj.one_shot_threads + obj.hierarchical_threads
    finished = obj.completed_threads + obj.failed_threads
    if obj.state == MEASURED:
        if assigned != obj.pending_threads:
            raise TelemetryError(
                f"{where}: one_shot+hierarchical ({assigned}) must equal"
                f" pending_threads ({obj.pending_threads})")
        if finished != obj.pending_threads:
            raise TelemetryError(
                f"{where}: completed+failed ({finished}) must equal"
                f" pending_threads ({obj.pending_threads})")
    elif assigned > obj.pending_threads or finished > obj.pending_threads:
        raise TelemetryError(
            f"{where}: a partial record may not claim more assignments or"
            f" outcomes than its {obj.pending_threads} pending threads")


_QUEUE_COUNTS = (
    "pending_entities", "input_tokens", "bucket_count", "microbatch_count",
    "padding_tokens", "successful_entities", "failed_entities",
    "individual_fallbacks", "bisection_fallbacks",
)


def _validate_embed(obj: EmbedTelemetry) -> None:
    where = "performance.embed"
    _check_state(obj.state, f"{where}.state")
    for name in ("leaf", "summary"):
        _check_counts(getattr(obj.queues, name), _QUEUE_COUNTS,
                      f"{where}.queues.{name}")
    _check_count(obj.verified_cache_publications,
                 f"{where}.verified_cache_publications")
    _check_timings(obj.timings_seconds, f"{where}.timings_seconds")
    if obj.state in (NOT_RUN, NOT_APPLICABLE):
        _require_zero(obj, where)
        return
    successful = 0
    for name in ("leaf", "summary"):
        queue = getattr(obj.queues, name)
        outcomes = queue.successful_entities + queue.failed_entities
        successful += queue.successful_entities
        if obj.state == MEASURED and outcomes != queue.pending_entities:
            raise TelemetryError(
                f"{where}.queues.{name}: successful+failed ({outcomes}) must"
                f" equal pending_entities ({queue.pending_entities})")
        if obj.state == PARTIAL and outcomes > queue.pending_entities:
            raise TelemetryError(
                f"{where}.queues.{name}: a partial record may not claim more"
                f" outcomes than its {queue.pending_entities} pending"
                " entities")
    if obj.state == MEASURED \
            and obj.verified_cache_publications != successful:
        raise TelemetryError(
            f"{where}: verified_cache_publications"
            f" ({obj.verified_cache_publications}) must equal successful"
            f" entities across both queues ({successful})")
    if obj.state == PARTIAL and obj.verified_cache_publications > successful:
        raise TelemetryError(
            f"{where}: verified_cache_publications may not exceed successful"
            " entities")


_PDF_COUNTS = (
    "occurrences_considered", "pending_occurrences", "unique_transforms",
    "successful_transforms", "failed_transforms", "duplicate_reuses",
    "direct_original_fallbacks",
)


def _validate_pdfs(obj: PdfsTelemetry) -> None:
    where = "performance.pdfs"
    _check_state(obj.state, f"{where}.state")
    _check_counts(obj, _PDF_COUNTS, where)
    _check_counts(obj.fan_out, ("copies", "copy_on_write_clones"),
                  f"{where}.fan_out")
    resources = obj.resources
    _check_counts(resources, (
        "configured_worker_count", "configured_per_child_jobs",
        "configured_global_cpu_budget", "observed_peak_workers"),
        f"{where}.resources")
    if resources.process_tree_peak_rss_bytes is not None:
        _check_count(resources.process_tree_peak_rss_bytes,
                     f"{where}.resources.process_tree_peak_rss_bytes")
    if resources.configured_worker_count \
            * resources.configured_per_child_jobs \
            > resources.configured_global_cpu_budget:
        raise TelemetryError(
            f"{where}: workers * per-child jobs exceeds global CPU budget")
    if resources.observed_peak_workers > resources.configured_worker_count:
        raise TelemetryError(
            f"{where}: observed peak workers exceeds configured workers")
    _check_timings(obj.timings_seconds, f"{where}.timings_seconds")
    if obj.state in (NOT_RUN, NOT_APPLICABLE):
        _require_zero(obj, where)
        return
    outcomes = obj.successful_transforms + obj.failed_transforms
    if obj.state == MEASURED and outcomes != obj.unique_transforms:
        raise TelemetryError(
            f"{where}: successful+failed transforms ({outcomes}) must equal"
            f" unique_transforms ({obj.unique_transforms})")
    if obj.state == MEASURED and (
            obj.unique_transforms > obj.pending_occurrences
            or obj.duplicate_reuses !=
            obj.pending_occurrences - obj.unique_transforms):
        raise TelemetryError(
            f"{where}: pending occurrences must equal unique transforms plus"
            " duplicate reuses")
    if obj.state == PARTIAL and outcomes > obj.unique_transforms:
        raise TelemetryError(
            f"{where}: a partial record may not claim more transform"
            f" outcomes than its {obj.unique_transforms} unique transforms")


# -- strict JSON loading -----------------------------------------------------


def _require_exact_keys(data: Any, expected: tuple[str, ...],
                        where: str) -> dict[str, Any]:
    if not isinstance(data, dict):
        raise TelemetryError(f"{where} must be an object, got"
                             f" {type(data).__name__}")
    actual = set(data)
    unknown = actual - set(expected)
    missing = set(expected) - actual
    if unknown:
        raise TelemetryError(
            f"{where}: unknown fields {sorted(unknown)}")
    if missing:
        raise TelemetryError(
            f"{where}: missing required fields {sorted(missing)}")
    return data


def _load_flat(cls, data: Any, where: str):
    """Strict load for a dataclass of scalar fields only."""
    names = tuple(spec.name for spec in fields(cls))
    payload = _require_exact_keys(data, names, where)
    return cls(**{name: payload[name] for name in names})


def _load_summaries(data: Any) -> SummariesTelemetry:
    where = "performance.summaries"
    names = tuple(spec.name for spec in fields(SummariesTelemetry))
    payload = dict(_require_exact_keys(data, names, where))
    tiers = payload["length_tiers"]
    if not isinstance(tiers, list):
        raise TelemetryError(f"{where}.length_tiers must be an array")
    payload["length_tiers"] = [
        _load_flat(LengthTier, tier, f"{where}.length_tiers[{index}]")
        for index, tier in enumerate(tiers)]
    payload["timings_seconds"] = _load_flat(
        SummariesTimings, payload["timings_seconds"],
        f"{where}.timings_seconds")
    return SummariesTelemetry(**payload)


def _load_embed(data: Any) -> EmbedTelemetry:
    where = "performance.embed"
    names = tuple(spec.name for spec in fields(EmbedTelemetry))
    payload = dict(_require_exact_keys(data, names, where))
    queues = _require_exact_keys(
        payload["queues"], ("leaf", "summary"), f"{where}.queues")
    payload["queues"] = EmbedQueues(
        leaf=_load_flat(EmbedQueueTelemetry, queues["leaf"],
                        f"{where}.queues.leaf"),
        summary=_load_flat(EmbedQueueTelemetry, queues["summary"],
                           f"{where}.queues.summary"))
    payload["timings_seconds"] = _load_flat(
        EmbedTimings, payload["timings_seconds"], f"{where}.timings_seconds")
    return EmbedTelemetry(**payload)


def _load_pdfs(data: Any) -> PdfsTelemetry:
    where = "performance.pdfs"
    names = tuple(spec.name for spec in fields(PdfsTelemetry))
    payload = dict(_require_exact_keys(data, names, where))
    payload["fan_out"] = _load_flat(
        PdfFanOut, payload["fan_out"], f"{where}.fan_out")
    payload["resources"] = _load_flat(
        PdfResources, payload["resources"], f"{where}.resources")
    payload["timings_seconds"] = _load_flat(
        PdfsTimings, payload["timings_seconds"], f"{where}.timings_seconds")
    return PdfsTelemetry(**payload)


def performance_from_json(data: Any) -> PerformanceTelemetry:
    """Strictly load and validate one persisted performance block."""
    payload = _require_exact_keys(data, HOT_STAGE_NAMES, "performance")
    performance = PerformanceTelemetry(
        summaries=_load_summaries(payload["summaries"]),
        embed=_load_embed(payload["embed"]),
        pdfs=_load_pdfs(payload["pdfs"]),
    )
    performance.validate()
    return performance
