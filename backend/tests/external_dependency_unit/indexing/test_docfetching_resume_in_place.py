"""Regression coverage for checkpoint-resume batch handling.

A leftover batch from a prior (failed) attempt must be re-issued *in place* —
docprocessing reads it from the attempt that wrote it — instead of being copied
into the resuming attempt's namespace. The copy was one S3 round-trip per file
and, on large piles, stalled resume past the heartbeat window so the connector
never advanced. This runs against the real file store because the guarantee is
about where bytes physically live, not just which task kwargs get sent.
"""

from unittest.mock import MagicMock

from sqlalchemy.orm import Session

from onyx.background.indexing.run_docfetching import reissue_old_batches
from onyx.configs.constants import DocumentSource
from onyx.configs.constants import OnyxCeleryPriority
from onyx.connectors.models import Document
from onyx.connectors.models import TextSection
from onyx.file_store.document_batch_storage import get_document_batch_storage
from tests.external_dependency_unit.constants import TEST_TENANT_ID
from tests.external_dependency_unit.indexing_helpers import cleanup_cc_pair
from tests.external_dependency_unit.indexing_helpers import make_cc_pair


def _make_doc(doc_id: str) -> Document:
    return Document(
        id=doc_id,
        source=DocumentSource.MOCK_CONNECTOR,
        semantic_identifier=doc_id,
        sections=[TextSection(text="payload", link=None)],
        metadata={},
    )


def test_resume_reissues_leftover_batches_in_place(
    db_session: Session,
    tenant_context: None,  # noqa: ARG001
    initialize_file_store: None,  # noqa: ARG001
) -> None:
    cc_pair = make_cc_pair(db_session)
    source_attempt_id = 4242
    new_attempt_id = 4243
    batch_num = 7

    source_storage = get_document_batch_storage(cc_pair.id, source_attempt_id)
    new_storage = get_document_batch_storage(cc_pair.id, new_attempt_id)
    try:
        # A prior attempt wrote one batch, then failed before it was processed.
        source_storage.store_batch(batch_num, [_make_doc("doc-a")])

        # Resume under a new attempt; capture what gets enqueued.
        app = MagicMock()
        app.send_task.return_value = MagicMock()
        reissued, recent = reissue_old_batches(
            batch_storage=new_storage,
            index_attempt_id=new_attempt_id,
            cc_pair_id=cc_pair.id,
            tenant_id=TEST_TENANT_ID,
            app=app,
            most_recent_attempt=None,
            priority=OnyxCeleryPriority.MEDIUM,
        )

        assert reissued == 1  # the one leftover batch
        assert recent == 0  # no prior attempt completed any batches

        # The batch stays where the source attempt wrote it — not copied into
        # the resuming attempt's namespace.
        assert new_storage.get_batch(batch_num) is None
        source_docs = source_storage.get_batch(batch_num)
        assert source_docs is not None
        assert [doc.id for doc in source_docs] == ["doc-a"]

        # The docprocessing task is pointed at the source attempt so it reads
        # the batch in place, while all bookkeeping stays on the new attempt.
        app.send_task.assert_called_once()
        task_kwargs = app.send_task.call_args.kwargs["kwargs"]
        assert task_kwargs["index_attempt_id"] == new_attempt_id
        assert task_kwargs["source_index_attempt_id"] == source_attempt_id
        assert task_kwargs["batch_num"] == batch_num

        # Mirror how _docprocessing_task routes storage from those kwargs: the
        # batch is read from — and deleted in — the source attempt's namespace.
        owner_attempt_id = (
            task_kwargs["source_index_attempt_id"]
            if task_kwargs["source_index_attempt_id"] is not None
            else task_kwargs["index_attempt_id"]
        )
        read_storage = get_document_batch_storage(
            task_kwargs["cc_pair_id"], owner_attempt_id
        )
        read_docs = read_storage.get_batch(batch_num)
        assert read_docs is not None
        assert [doc.id for doc in read_docs] == ["doc-a"]
        read_storage.delete_batch_by_num(batch_num)
        assert read_storage.get_batch(batch_num) is None
    finally:
        # cleanup_all_batches lists by cc_pair prefix, so it drains every
        # attempt's leftovers for this cc_pair regardless of namespace.
        new_storage.cleanup_all_batches()
        cleanup_cc_pair(db_session, cc_pair)
