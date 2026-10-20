-- The two model classes (Sebastian, 2026-10-20): every purpose alias of
-- before was a name for big or small, so history reads in the classes.
-- The audit row, a running card's model and a thread's compaction model.
-- The ledger is immutable: its refs keep the old names, and the metrics
-- read them as their classes.
UPDATE audit SET model_alias = 'big' WHERE model_alias IN ('coding-frontier', 'review-frontier', 'supervisor', 'checkin', 'operator');
UPDATE audit SET model_alias = 'small' WHERE model_alias IN ('coding-cheap', 'pulse');
UPDATE tasks SET model_alias = 'big' WHERE model_alias IN ('coding-frontier', 'review-frontier');
UPDATE tasks SET model_alias = 'small' WHERE model_alias IN ('coding-cheap', 'pulse');
UPDATE chat_conversations SET summary_model = 'small' WHERE summary_model = 'pulse';
