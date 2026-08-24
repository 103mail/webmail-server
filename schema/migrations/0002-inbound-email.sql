-- Add the table that makes inbound delivery idempotent.
--
-- SNS delivers at least once, so the same received message can be posted to the
-- webhook more than once. Without a record of what has been processed, a
-- redelivery creates a second thread carrying the same message.
--
-- This table is additive. No existing table changes, so the PHP backend is
-- unaffected and can keep running against the same database.

CREATE TABLE `inbound_email` (
  `provider_id` varchar(255) NOT NULL,
  `received_at` datetime NOT NULL DEFAULT current_timestamp(),
  PRIMARY KEY (`provider_id`),
  KEY `received_at` (`received_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
