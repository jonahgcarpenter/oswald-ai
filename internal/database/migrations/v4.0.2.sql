ALTER TABLE durable_jobs
ADD COLUMN invalid_output_retry_count INTEGER NOT NULL DEFAULT 0
CHECK (
    invalid_output_retry_count = 0
    OR (
        job_kind = 'memory_formation'
        AND invalid_output_retry_count = 1
    )
);
