#!/usr/bin/env python3
"""Discrete-time survival predictor for KV-cache return times.

The model estimates a distribution over the time until the next cache-compatible
request. It deliberately does *not* choose a TTL: the returned probabilities are meant
to be consumed by a separate cost policy, which is the only place the rates live. That
split is the point — a model that picked a tier would bury the pricing assumption inside
the fit, where nobody can edit it.

It is the LEARNED counterpart of the hand-written arms in the `kvcache` Go package: the
two questions it answers, P(return within 5 minutes) and P(return within an hour), are
exactly the two `kvcache.Predictor.ReuseProbability(o, horizon)` is asked, so a service
that wants to use this wires it in behind that interface and nothing in the simulator or
the dashboard changes. See "Feeding the Go strategy seam" below.

Usage: kv_ttl_survival_predictor.py    # self-test on synthetic traffic; see HOW TO RUN IT
       imported as a library           # the normal case


WHICH DATA IT NEEDS
-------------------
One row per request, in a pandas DataFrame. Nothing else — no request bodies, no
transcripts, no token counts. The model is about TIMING.

Required, always:

    user_id           who owns the traffic. Any stable label.
    request_time      when the request was made. Parsed with utc=True, so pass either
                      tz-aware timestamps or UTC-naive ones; a naive LOCAL time will be
                      read as UTC and every hour-of-day feature will be wrong.

Required for fit() only, and normally produced by build_next_request_targets():

    time_to_next_request_seconds  seconds until the next cache-compatible request, or —
                                  for a right-censored row — how long the row was
                                  observed for.
    event_observed                True when a return was actually seen, False when the
                                  row is right-censored. Defaults to True when the
                                  column is absent, which is a trap: see (1) below.

Optional, and worth having:

    anything named in `categorical_features` (default `("user_id",)`) or in
    `numeric_features` (default empty). `model` and `agent` are the two that carry real
    signal on this deployment's traffic.

Everything else the model uses is ENGINEERED from the two required columns, per user and
strictly backwards in time — cyclical hour-of-day and weekday, the previous gap, a
rolling median and an EWMA of past gaps, the past return rate at 5/15/60 minutes, the
request counts in the previous 10 and 60 minutes, and how many requests the user has
made so far. No engineered feature reads a row later than its own, which is what makes a
backtest of this honest.


WHERE THAT DATA COMES FROM IN THIS PROJECT
------------------------------------------
context-guru's dashboard store already holds it: one row per proxied request in the
`requests` table of the SQLite database named by `DASHBOARD_DB` (the hosted deployment's
is `/var/lib/context-guru/cg.db`). The mapping is a rename and a unit conversion:

    user_id          <- requests.tenant_id      the owning account
    conversation_id  <- requests.session_id     the trajectory
    model            <- requests.model
    agent            <- requests.agent          claude-cli | litellm | anthropic | ...
    request_time     <- requests.ts             epoch MILLISECONDS, UTC
    upstream_ms      <- requests.upstream_ms    optional, for a latency-aware policy

Read it READ-ONLY. The extraction, in full:

    import sqlite3, pandas as pd
    con = sqlite3.connect("file:/var/lib/context-guru/cg.db?mode=ro", uri=True)
    raw = pd.read_sql_query(
        "SELECT tenant_id AS user_id, session_id AS conversation_id, model, agent, "
        "       ts AS request_time_ms, upstream_ms "
        "FROM requests "
        "WHERE session_id <> '' AND token_accounting <> 'missing' "
        "ORDER BY ts, id", con)
    raw["request_time"] = pd.to_datetime(raw.pop("request_time_ms"), unit="ms", utc=True)

Two filters in that WHERE clause, both deliberate. `session_id <> ''` drops rows with no
trajectory to belong to. `token_accounting <> 'missing'` drops rows the provider never
reported usage for. On a database that has the column, add `AND keepalive = 0`: a
keep-alive ping is a request context-guru sent on its own initiative while nobody was at
the keyboard, so counting it as a return would teach the model that the user came back
when the user did not. Older snapshots predate that column and have no pings in them.

Then label it — this is the step that turns a request log into survival data:

    events = build_next_request_targets(
        raw,
        compatibility_columns=("user_id", "conversation_id", "model"),
        observation_end=raw.request_time.max(),   # NOT optional; see (1)
    )


HOW TO RUN IT
-------------
Needs numpy, pandas, scikit-learn and joblib, none of which the rest of this repo uses.
Keep them out of the system interpreter:

    python3 -m venv .venv-kvpred
    .venv-kvpred/bin/pip install "numpy<2" pandas scikit-learn joblib
    .venv-kvpred/bin/python deploy/harbor/kv_ttl_survival_predictor.py

Run directly, the `__main__` block fits the model on synthetic two-regime traffic (a
short daytime scale, a long overnight one) and prints one request's distribution. It is
a self-test, not a benchmark: it exists so that a fresh checkout can prove the pipeline
fits, predicts, and returns rows that sum to one.

Verified on python 3.9.25 with numpy 1.26.4, pandas 2.3.3, scikit-learn 1.6.1 and
joblib 1.5.3.


HOW TO USE IT
-------------
    predictor = KVTTLTimePredictor(
        bucket_seconds=5 * 60,        # bucket 0 IS the 5-minute tier question
        max_ttl_seconds=60 * 60,      # the tail IS the 1-hour tier question
        categorical_features=("user_id", "model", "agent"),
    )
    predictor.fit(train_events)                    # .train() is the same call
    proba = predictor.predict_proba(test_events)   # (n_rows, n_buckets + 1)
    predictor.save("kvttl.joblib")
    predictor = KVTTLTimePredictor.load("kvttl.joblib")

`fit` and `train` are the same method. `predict_proba` and `inference` are the same
method. `predict_distribution` returns the same numbers in long form for a human, and
`predict_one(history, current_request)` is the single-request convenience — it appends
the request to the history, engineers features over the concatenation so nothing from
the future is used, and returns only that request's rows.

A cache that has ALREADY been idle is handled by conditioning, not by re-fitting:

    predictor.predict_proba(rows, elapsed_seconds=240)

conditions on survival through every FULLY COMPLETED bucket, so with five-minute buckets
an elapsed value anywhere in 0..299 s conditions on nothing and 300..599 s conditions on
T > 300 s. Call it at bucket boundaries, or use smaller buckets, or the conditioning is
coarser than the decision it feeds.

Choosing `bucket_seconds` and `max_ttl_seconds`: the defaults are not a round number
picked for tidiness, they are the provider's two tiers. With 300 s buckets and a 3600 s
horizon, column 0 is exactly P(return inside the 5-minute lifetime) and one minus the
tail is exactly P(return inside the 1-hour lifetime) — the two probabilities a TTL
decision needs, read straight off, with no interpolation. Choose finer buckets only if
something downstream needs the shape inside five minutes; `max_ttl_seconds` must stay an
exact multiple of `bucket_seconds` (the constructor enforces it).


INPUT AND OUTPUT
----------------
build_next_request_targets(events, compatibility_columns=..., observation_end=...)
    in   a DataFrame with the compatibility columns and `request_time`
    out  the same rows, sorted by (compatibility columns, request_time) with the index
         reset, plus:
           time_to_next_request_seconds  float; seconds to the next row of the same
                                         compatibility group. For the LAST row of each
                                         group: seconds to `observation_end`, or NaN
                                         when no `observation_end` was given.
           event_observed                bool; False for that last row of each group.
         Raises ValueError on a missing column, on empty compatibility_columns, or on an
         `observation_end` that precedes a request.

KVTTLTimePredictor.fit(requests) -> self
    Drops rows whose duration is NaN (unlabelled), engineers history over ALL rows
    including those, expands each surviving row into one logistic-regression example per
    bucket it was still at risk in, and fits. Afterwards `training_summary_` reports
    `request_rows_used`, `person_period_rows`, `events_within_horizon`, `bucket_seconds`
    and `max_ttl_seconds`. Raises ValueError if nothing is labelled, if a duration is
    negative, if no complete risk interval exists, or if the labels have only one class.

KVTTLTimePredictor.predict_hazards(requests) -> np.ndarray
    shape (n_rows, n_buckets); entry [i, k] is P(return in bucket k | survived to k),
    clipped into [1e-7, 1-1e-7]. A CONDITIONAL hazard — it does not sum to anything.

KVTTLTimePredictor.predict_proba(requests, elapsed_seconds=0.0) -> np.ndarray
    shape (n_rows, n_buckets + 1); a proper distribution, each row summing to 1.0.
    Columns 0..K-1 are the buckets (0, bucket_seconds], ..., and column K is the
    "later than max_ttl_seconds" tail. Buckets already completed per `elapsed_seconds`
    are exactly 0. With the default settings that is 13 columns:

        column   interval          what it answers
        0        (0, 300] s        P(return inside the 5-minute lifetime)
        1..11    (300, 3600] s     the shape between the two tiers
        12       > 3600 s          P(the 1-hour tier expires unused)
        sum(0..11) = 1 - column 12 = P(return inside the 1-hour lifetime)

KVTTLTimePredictor.predict_distribution(...) -> pd.DataFrame
    the same numbers, one row per (request, bucket), with `request_row`, `bucket_index`,
    `interval`, `start_seconds`, `end_seconds`, `probability`, `cumulative_probability`
    and `is_tail`. `end_seconds` is np.inf on the tail row.

KVTTLTimePredictor.predict_one(history, current_request, elapsed_seconds=0.0)
    -> the same frame, for the last row only, with `request_row` dropped.

One request's output, from the self-test, abbreviated:

    bucket_index             interval  probability  cumulative_probability  is_tail
               0     (0, 300] seconds     0.722863                0.722863    False
               1   (300, 600] seconds     0.213698                0.936561    False
               2   (600, 900] seconds     0.043136                0.979697    False
             ...
              12       > 3600 seconds     0.000006                1.000000     True


FEEDING THE GO STRATEGY SEAM
----------------------------
`kvcache.Predictor` in this repo asks one question — ReuseProbability(observation,
horizon) — and the two horizons it asks about are 5 minutes and 1 hour. Those are
column 0 and one-minus-the-tail above, which is why the default bucketing is what it is:

    proba = predictor.predict_proba(rows)
    p_5m  = proba[:, 0]
    p_1h  = 1.0 - proba[:, -1]

Serve those two numbers and `kvcache.Custom{Predictor: ...}` scores this model against
the same baseline, the same prices and the same savings arithmetic as every hand-written
arm. Nothing about the horizon set is baked in here: a third horizon is a third slice of
the same distribution.


MEASURED ON REAL TRAFFIC
------------------------
Against the hosted deployment's own capture — 14,407 requests, 12 accounts, 1,772
trajectories, 13 models, over the 57 hours from 2026-08-17 11:48 UTC — labelled on
(user_id, conversation_id, model) with `observation_end` at the last request:

    labelled rows        14,407
    observed returns     12,511
    right-censored        1,896   (13.2% of rows: the last request of each group)
    chronological split   70/30 -> 10,085 train / 4,322 test
    person-period rows    26,093 from the 10,085 training rows
    fit                   0.6 s
    predict 4,322 rows    0.27 s, every row summing to 1.000000

    horizon   actual   predicted   Brier    Brier of the base rate
    <= 5m     0.7558   0.8157      0.0986   0.1846
    <= 1h     0.8315   0.8587      0.0695   0.1401

So it roughly halves the Brier score of the constant "everyone comes back" guess, and it
discriminates rather than predicting the mean — P(return within 5m) across the test rows
runs 0.111 at the 5th percentile to 0.982 at the 95th. It also runs about six points
OPTIMISTIC on this split, because the training half of the window had a higher return
rate than the test half. Re-fit and re-check the calibration before trusting a threshold
on it; a policy tuned against a six-point bias will buy TTL it does not need.


CORRECTNESS NOTES, EACH OF WHICH IS A WAY TO GET A WRONG ANSWER QUIETLY
----------------------------------------------------------------------
(1) `observation_end` is not optional in practice. Without it, the last request of every
    compatibility group gets a NaN duration and `fit` silently drops it — and those are
    precisely the rows that had NOT come back. On the production window that is 1,896
    of 14,407 rows, 13.2%, all of them at the long end, so omitting it biases the model
    toward short gaps. `event_observed` matters for the same reason and in the same
    direction: it defaults to True when the column is absent, which would train a
    censored row as a confirmed return.

(2) `compatibility_columns` must name everything a KV cache entry is keyed on, and that
    includes the MODEL. An entry does not transfer between models, and 101 of this
    deployment's 1,772 trajectories use more than one — keying on (user, conversation)
    alone merges them and derives a return that could not have hit any cache. Keying on
    the conversation without the user is worse: the session id is client-supplied, so two
    accounts can present the same one, and the groups would splice their traffic
    together. Adding the model splits 1,772 conversations into 1,896 groups here.

(3) The history features are per USER, the targets are per compatibility GROUP. That is
    deliberate — how busy someone has been is informative about whether they will come
    back to this conversation — but it means `previous_gap_seconds` can refer to a
    different conversation of the same user. It is still strictly past information, so it
    is not leakage; it is a modelling choice, and it is the one to revisit first if a
    per-conversation variant is wanted.

(4) Split by TIME, never at random. Every engineered feature is backward-looking, so a
    single row leaks nothing — but the coefficients are fitted over the whole frame it is
    given, so scoring on rows inside that frame is in-sample and will flatter the model.
    The numbers above come from a chronological cut for exactly this reason.

(5) Judge it by CALIBRATION, not accuracy. 75.6% of production rows return within five
    minutes, so "yes" is right three times in four while knowing nothing. Brier score
    against the base rate (above) or a reliability curve is the comparison that means
    something; a hazard model's value is the probability, not the label.

(6) `predict_one` re-engineers features over history + the new request on every call, so
    it is O(len(history)) per prediction. Fine interactively, wrong in a request path and
    wrong in a backtest — pass the whole frame to `predict_proba` and read the rows.

(7) `elapsed_seconds` conditions on completed buckets only, so it is exact at bucket
    boundaries and rounds DOWN in between. With the default 300 s buckets, asking at 299
    seconds idle conditions on nothing at all.


Example
-------
>>> predictor = KVTTLTimePredictor(
...     bucket_seconds=5 * 60,
...     max_ttl_seconds=60 * 60,
...     categorical_features=("user_id", "model"),
... )
>>> predictor.fit(training_requests)
>>> distribution = predictor.predict_one(history, current_request)
>>> print(distribution[["interval", "probability"]])

``history`` excludes the current request. ``current_request`` is a mapping or Series
containing the request-time features used during training.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

import joblib
import numpy as np
import pandas as pd
from sklearn.compose import ColumnTransformer
from sklearn.base import BaseEstimator
from sklearn.impute import SimpleImputer
from sklearn.linear_model import LogisticRegression
from sklearn.pipeline import Pipeline
from sklearn.preprocessing import OneHotEncoder, StandardScaler
from sklearn.utils.validation import check_is_fitted


def build_next_request_targets(
    events: pd.DataFrame,
    *,
    compatibility_columns: Sequence[str],
    time_column: str = "request_time",
    observation_end: str | pd.Timestamp | None = None,
    duration_column: str = "time_to_next_request_seconds",
    event_column: str = "event_observed",
) -> pd.DataFrame:
    """Add next-compatible-request durations to a request-event table.

    ``compatibility_columns`` must identify requests that can reuse the same
    cache, for example ``("user_id", "conversation_id", "model")``.

    The final event in each compatibility group is right-censored when
    ``observation_end`` is supplied. Without it, its duration is unknown and the
    predictor will omit that row during fitting.
    """

    required = [*compatibility_columns, time_column]
    missing = [column for column in required if column not in events.columns]
    if missing:
        raise ValueError(f"Missing required columns: {missing}")
    if not compatibility_columns:
        raise ValueError("compatibility_columns must contain at least one column")

    result = events.copy()
    result[time_column] = pd.to_datetime(result[time_column], utc=True)
    result = result.sort_values([*compatibility_columns, time_column]).reset_index(drop=True)

    grouped = result.groupby(list(compatibility_columns), sort=False, dropna=False)
    next_time = grouped[time_column].shift(-1)
    result[duration_column] = (next_time - result[time_column]).dt.total_seconds()
    result[event_column] = next_time.notna()

    if observation_end is not None:
        end = pd.to_datetime(observation_end, utc=True)
        censored = ~result[event_column]
        censor_durations = (end - result.loc[censored, time_column]).dt.total_seconds()
        if (censor_durations < 0).any():
            raise ValueError("observation_end precedes one or more request times")
        result.loc[censored, duration_column] = censor_durations

    return result


class KVTTLTimePredictor(BaseEstimator):
    """Discrete-time logistic-hazard model for the next request time.

    One logistic-regression example is created for each request/bucket for
    which the cache was still at risk. The predicted conditional hazards are
    converted into a proper distribution whose probabilities sum to one.

    User identity is handled by a shared model with one-hot user effects. This
    is generally more stable than fitting a separate model for each user, while
    still allowing personalized predictions.
    """

    _ENGINEERED_NUMERIC = (
        "request_hour_sin",
        "request_hour_cos",
        "request_weekday_sin",
        "request_weekday_cos",
        "previous_gap_seconds",
        "rolling_gap_median_seconds",
        "ewma_gap_seconds",
        "past_return_rate_5m",
        "past_return_rate_15m",
        "past_return_rate_60m",
        "requests_in_previous_10m",
        "requests_in_previous_60m",
        "user_history_count",
        "decision_hour_sin",
        "decision_hour_cos",
    )

    def __init__(
        self,
        *,
        bucket_seconds: int = 5 * 60,
        max_ttl_seconds: int = 60 * 60,
        user_column: str = "user_id",
        time_column: str = "request_time",
        duration_column: str = "time_to_next_request_seconds",
        event_column: str = "event_observed",
        categorical_features: Sequence[str] = ("user_id",),
        numeric_features: Sequence[str] = (),
        history_window: int = 20,
        regularization_c: float = 1.0,
        random_state: int = 0,
    ) -> None:
        if bucket_seconds <= 0:
            raise ValueError("bucket_seconds must be positive")
        if max_ttl_seconds <= 0 or max_ttl_seconds % bucket_seconds != 0:
            raise ValueError("max_ttl_seconds must be a positive multiple of bucket_seconds")
        if history_window <= 0:
            raise ValueError("history_window must be positive")

        self.bucket_seconds = int(bucket_seconds)
        self.max_ttl_seconds = int(max_ttl_seconds)
        self.user_column = user_column
        self.time_column = time_column
        self.duration_column = duration_column
        self.event_column = event_column
        self.categorical_features = tuple(categorical_features)
        self.numeric_features = tuple(numeric_features)
        self.history_window = int(history_window)
        self.regularization_c = float(regularization_c)
        self.random_state = int(random_state)

    @property
    def n_buckets(self) -> int:
        return self.max_ttl_seconds // self.bucket_seconds

    @property
    def bucket_edges_seconds(self) -> np.ndarray:
        return np.arange(self.n_buckets + 1, dtype=float) * self.bucket_seconds

    def fit(self, requests: pd.DataFrame) -> "KVTTLTimePredictor":
        """Fit the hazard model from chronologically observed requests."""

        self._validate_request_columns(requests, training=True)
        frame = requests.copy().reset_index(drop=True)
        frame[self.time_column] = pd.to_datetime(frame[self.time_column], utc=True)
        frame[self.duration_column] = pd.to_numeric(frame[self.duration_column], errors="coerce")

        if self.event_column not in frame.columns:
            frame[self.event_column] = True
        frame[self.event_column] = frame[self.event_column].astype(bool)

        known = frame[self.duration_column].notna()
        if not known.any():
            raise ValueError("No rows have a known event or censoring duration")
        if (frame.loc[known, self.duration_column] < 0).any():
            raise ValueError("Durations must be non-negative")

        # Engineer history on every request, including rows whose own target is
        # unknown: those requests are still legitimate history for later rows.
        all_request_features = self._engineer_request_features(frame)
        request_features = all_request_features.loc[known].reset_index(drop=True)
        training_frame = frame.loc[known].reset_index(drop=True)
        person_period, labels = self._expand_training_rows(
            request_features,
            training_frame[self.duration_column].to_numpy(dtype=float),
            training_frame[self.event_column].to_numpy(dtype=bool),
        )
        if person_period.empty:
            raise ValueError("No complete risk intervals are available for training")
        if np.unique(labels).size < 2:
            raise ValueError(
                "Training data needs both a return event within the modeled horizon "
                "and at least one survived bucket"
            )

        self.numeric_columns_ = [*self._ENGINEERED_NUMERIC, *self.numeric_features]
        self.categorical_columns_ = [*self.categorical_features, "bucket_index"]
        self.model_columns_ = [*self.numeric_columns_, *self.categorical_columns_]

        numeric_pipeline = Pipeline(
            steps=[
                ("imputer", SimpleImputer(strategy="median", add_indicator=True)),
                ("scaler", StandardScaler()),
            ]
        )
        categorical_pipeline = Pipeline(
            steps=[
                ("imputer", SimpleImputer(strategy="most_frequent")),
                ("one_hot", OneHotEncoder(handle_unknown="ignore")),
            ]
        )
        preprocessor = ColumnTransformer(
            transformers=[
                ("numeric", numeric_pipeline, self.numeric_columns_),
                ("categorical", categorical_pipeline, self.categorical_columns_),
            ]
        )
        classifier = LogisticRegression(
            C=self.regularization_c,
            max_iter=2_000,
            random_state=self.random_state,
        )
        self.pipeline_ = Pipeline(
            steps=[("preprocessor", preprocessor), ("classifier", classifier)]
        )
        self.pipeline_.fit(person_period[self.model_columns_], labels)

        self.training_summary_ = {
            "request_rows_used": int(len(training_frame)),
            "person_period_rows": int(len(person_period)),
            "events_within_horizon": int(labels.sum()),
            "bucket_seconds": self.bucket_seconds,
            "max_ttl_seconds": self.max_ttl_seconds,
        }
        return self

    # ``train`` is included because it is often the most natural API name in a
    # service. ``fit`` remains sklearn-compatible.
    def train(self, requests: pd.DataFrame) -> "KVTTLTimePredictor":
        return self.fit(requests)

    def predict_hazards(self, requests: pd.DataFrame) -> np.ndarray:
        """Return ``P(event in bucket k | survived to bucket k)``.

        The output shape is ``(number of requests, number of buckets)``.
        """

        check_is_fitted(self, "pipeline_")
        self._validate_request_columns(requests, training=False)
        frame = requests.copy().reset_index(drop=True)
        frame[self.time_column] = pd.to_datetime(frame[self.time_column], utc=True)
        request_features = self._engineer_request_features(frame)
        risk_rows = self._make_all_risk_rows(request_features)

        hazards = self.pipeline_.predict_proba(risk_rows[self.model_columns_])[:, 1]
        hazards = np.clip(hazards, 1e-7, 1.0 - 1e-7)
        return hazards.reshape(len(frame), self.n_buckets)

    def predict_proba(
        self,
        requests: pd.DataFrame,
        *,
        elapsed_seconds: float = 0.0,
    ) -> np.ndarray:
        """Return probabilities for every bucket plus the ``> horizon`` tail.

        Columns 0..K-1 represent the fixed time buckets and column K represents
        a return later than ``max_ttl_seconds``. Rows sum to one.

        If the cache has already been idle, ``elapsed_seconds`` conditions on
        survival through all fully completed buckets. With five-minute buckets,
        an elapsed value of 12 minutes therefore conditions on ``T > 10m``.
        TTL decisions should normally call this at bucket boundaries.
        """

        if elapsed_seconds < 0:
            raise ValueError("elapsed_seconds must be non-negative")

        hazards = self.predict_hazards(requests)
        completed = min(int(elapsed_seconds // self.bucket_seconds), self.n_buckets)
        probabilities = np.zeros((len(requests), self.n_buckets + 1), dtype=float)

        if completed == self.n_buckets:
            probabilities[:, -1] = 1.0
            return probabilities

        survival = np.ones(len(requests), dtype=float)
        for bucket in range(completed, self.n_buckets):
            probabilities[:, bucket] = survival * hazards[:, bucket]
            survival *= 1.0 - hazards[:, bucket]
        probabilities[:, -1] = survival
        return probabilities

    # Friendly alias for service code.
    def inference(
        self,
        requests: pd.DataFrame,
        *,
        elapsed_seconds: float = 0.0,
    ) -> np.ndarray:
        return self.predict_proba(requests, elapsed_seconds=elapsed_seconds)

    def predict_distribution(
        self,
        requests: pd.DataFrame,
        *,
        elapsed_seconds: float = 0.0,
    ) -> pd.DataFrame:
        """Return a readable long-form probability distribution."""

        probabilities = self.predict_proba(requests, elapsed_seconds=elapsed_seconds)
        records: list[dict[str, Any]] = []
        for request_row in range(len(requests)):
            cumulative = 0.0
            for bucket in range(self.n_buckets):
                start = int(bucket * self.bucket_seconds)
                end = int((bucket + 1) * self.bucket_seconds)
                probability = float(probabilities[request_row, bucket])
                cumulative += probability
                records.append(
                    {
                        "request_row": request_row,
                        "bucket_index": bucket,
                        "interval": f"({start}, {end}] seconds",
                        "start_seconds": start,
                        "end_seconds": end,
                        "probability": probability,
                        "cumulative_probability": cumulative,
                        "is_tail": False,
                    }
                )

            tail_probability = float(probabilities[request_row, -1])
            records.append(
                {
                    "request_row": request_row,
                    "bucket_index": self.n_buckets,
                    "interval": f"> {self.max_ttl_seconds} seconds",
                    "start_seconds": self.max_ttl_seconds,
                    "end_seconds": np.inf,
                    "probability": tail_probability,
                    "cumulative_probability": cumulative + tail_probability,
                    "is_tail": True,
                }
            )
        return pd.DataFrame.from_records(records)

    def predict_one(
        self,
        history: pd.DataFrame,
        current_request: Mapping[str, Any] | pd.Series,
        *,
        elapsed_seconds: float = 0.0,
    ) -> pd.DataFrame:
        """Predict for one new request using its user's preceding history.

        ``history`` may contain multiple users. The current request is appended,
        history features are calculated without future information, and only the
        current request's distribution is returned.
        """

        current = pd.DataFrame([dict(current_request)])
        combined = pd.concat([history, current], ignore_index=True, sort=False)
        distribution = self.predict_distribution(
            combined, elapsed_seconds=elapsed_seconds
        )
        last_row = len(combined) - 1
        return (
            distribution.loc[distribution["request_row"] == last_row]
            .drop(columns="request_row")
            .reset_index(drop=True)
        )

    def save(self, path: str | Path) -> None:
        """Serialize the fitted predictor with joblib."""

        check_is_fitted(self, "pipeline_")
        joblib.dump(self, Path(path))

    @classmethod
    def load(cls, path: str | Path) -> "KVTTLTimePredictor":
        """Load a predictor previously written by :meth:`save`."""

        predictor = joblib.load(Path(path))
        if not isinstance(predictor, cls):
            raise TypeError(f"{path!s} does not contain a {cls.__name__}")
        return predictor

    def _validate_request_columns(self, requests: pd.DataFrame, *, training: bool) -> None:
        required = {
            self.user_column,
            self.time_column,
            *self.categorical_features,
            *self.numeric_features,
        }
        if training:
            required.add(self.duration_column)
        missing = sorted(required.difference(requests.columns))
        if missing:
            raise ValueError(f"Missing required columns: {missing}")

    def _engineer_request_features(self, requests: pd.DataFrame) -> pd.DataFrame:
        frame = requests.copy().reset_index(drop=True)
        frame["_original_position"] = np.arange(len(frame))
        frame = frame.sort_values(
            [self.user_column, self.time_column, "_original_position"],
            kind="stable",
        ).reset_index(drop=True)

        timestamp = frame[self.time_column]
        seconds_of_day = (
            timestamp.dt.hour * 3600
            + timestamp.dt.minute * 60
            + timestamp.dt.second
            + timestamp.dt.microsecond / 1_000_000
        )
        frame["request_hour_sin"] = np.sin(2 * np.pi * seconds_of_day / 86_400)
        frame["request_hour_cos"] = np.cos(2 * np.pi * seconds_of_day / 86_400)
        weekday = timestamp.dt.dayofweek
        frame["request_weekday_sin"] = np.sin(2 * np.pi * weekday / 7)
        frame["request_weekday_cos"] = np.cos(2 * np.pi * weekday / 7)

        grouped = frame.groupby(self.user_column, sort=False, dropna=False)
        frame["previous_gap_seconds"] = grouped[self.time_column].diff().dt.total_seconds()
        previous_gap = frame["previous_gap_seconds"]

        rolling_group = previous_gap.groupby(frame[self.user_column], dropna=False)
        frame["rolling_gap_median_seconds"] = rolling_group.transform(
            lambda values: values.rolling(self.history_window, min_periods=1).median()
        )
        frame["ewma_gap_seconds"] = rolling_group.transform(
            lambda values: values.ewm(span=self.history_window, adjust=False).mean()
        )
        for minutes in (5, 15, 60):
            indicator = previous_gap.le(minutes * 60).where(previous_gap.notna())
            indicator_group = indicator.groupby(frame[self.user_column], dropna=False)
            frame[f"past_return_rate_{minutes}m"] = indicator_group.transform(
                lambda values: values.rolling(self.history_window, min_periods=1).mean()
            )

        frame["requests_in_previous_10m"] = self._counts_in_previous_window(
            frame, window_seconds=10 * 60
        )
        frame["requests_in_previous_60m"] = self._counts_in_previous_window(
            frame, window_seconds=60 * 60
        )
        frame["user_history_count"] = grouped.cumcount().astype(float)

        return (
            frame.sort_values("_original_position", kind="stable")
            .drop(columns="_original_position")
            .reset_index(drop=True)
        )

    def _counts_in_previous_window(
        self, frame: pd.DataFrame, *, window_seconds: int
    ) -> np.ndarray:
        counts = np.zeros(len(frame), dtype=float)
        for _, positions in frame.groupby(
            self.user_column, sort=False, dropna=False
        ).indices.items():
            positions = np.asarray(positions, dtype=int)
            nanoseconds = frame.loc[positions, self.time_column].astype("int64").to_numpy()
            left = np.searchsorted(
                nanoseconds,
                nanoseconds - window_seconds * 1_000_000_000,
                side="left",
            )
            counts[positions] = np.arange(len(positions)) - left
        return counts

    def _add_bucket_features(
        self, repeated: pd.DataFrame, bucket_indices: np.ndarray
    ) -> pd.DataFrame:
        result = repeated.copy()
        result["bucket_index"] = bucket_indices.astype(int)
        decision_time = result[self.time_column] + pd.to_timedelta(
            bucket_indices * self.bucket_seconds, unit="s"
        )
        decision_seconds = (
            decision_time.dt.hour * 3600
            + decision_time.dt.minute * 60
            + decision_time.dt.second
        )
        result["decision_hour_sin"] = np.sin(2 * np.pi * decision_seconds / 86_400)
        result["decision_hour_cos"] = np.cos(2 * np.pi * decision_seconds / 86_400)
        return result

    def _expand_training_rows(
        self,
        request_features: pd.DataFrame,
        durations: np.ndarray,
        observed: np.ndarray,
    ) -> tuple[pd.DataFrame, np.ndarray]:
        row_ids: list[int] = []
        bucket_ids: list[int] = []
        labels: list[int] = []
        upper_edges = self.bucket_edges_seconds[1:]

        for row_id, (duration, is_observed) in enumerate(zip(durations, observed)):
            if is_observed and duration <= self.max_ttl_seconds:
                event_bucket = int(np.searchsorted(upper_edges, duration, side="left"))
                for bucket in range(event_bucket + 1):
                    row_ids.append(row_id)
                    bucket_ids.append(bucket)
                    labels.append(int(bucket == event_bucket))
                continue

            if is_observed:
                complete_buckets = self.n_buckets
            else:
                complete_buckets = min(
                    int(duration // self.bucket_seconds), self.n_buckets
                )
            for bucket in range(complete_buckets):
                row_ids.append(row_id)
                bucket_ids.append(bucket)
                labels.append(0)

        if not row_ids:
            return pd.DataFrame(), np.asarray([], dtype=int)
        repeated = request_features.iloc[row_ids].reset_index(drop=True)
        expanded = self._add_bucket_features(repeated, np.asarray(bucket_ids))
        return expanded, np.asarray(labels, dtype=int)

    def _make_all_risk_rows(self, request_features: pd.DataFrame) -> pd.DataFrame:
        row_ids = np.repeat(np.arange(len(request_features)), self.n_buckets)
        bucket_ids = np.tile(np.arange(self.n_buckets), len(request_features))
        repeated = request_features.iloc[row_ids].reset_index(drop=True)
        return self._add_bucket_features(repeated, bucket_ids)


if __name__ == "__main__":
    # Small executable example. In real data, define compatibility using all
    # fields required for KV reuse, not merely user_id.
    rng = np.random.default_rng(7)
    rows: list[dict[str, Any]] = []
    for user_id in range(8):
        current_time = pd.Timestamp("2026-01-01", tz="UTC")
        for _ in range(100):
            hour = current_time.hour
            scale = 4 * 60 if 8 <= hour <= 20 else 18 * 60
            gap = float(rng.exponential(scale=scale))
            rows.append(
                {
                    "user_id": f"user-{user_id}",
                    "model": "large" if user_id % 2 else "small",
                    "request_time": current_time,
                    "time_to_next_request_seconds": gap,
                    "event_observed": True,
                }
            )
            current_time += pd.to_timedelta(gap, unit="s")

    example = pd.DataFrame(rows)
    model = KVTTLTimePredictor(categorical_features=("user_id", "model"))
    model.train(example)

    current = {
        "user_id": "user-1",
        "model": "large",
        "request_time": (
            example.loc[example["user_id"] == "user-1", "request_time"].max()
            + pd.Timedelta(minutes=3)
        ),
    }
    print(model.predict_one(example, current).to_string(index=False))
