-- 0012_notify_cursor: the local-notifications cursor (§10): the id of
-- the last event the delivery loop handed to the notifier, so a restart
-- neither re-notifies old events nor misses new ones. It lives in meta,
-- the store-wide singleton state that belongs to no other table. The
-- loop advances it past every event it read, notified or not, so a
-- broken notification never repeats.
ALTER TABLE meta ADD COLUMN notify_cursor INTEGER;
-- An existing store arrives here with a log full of events nobody ever
-- notified about; starting the cursor at 0 would replay all of them as
-- banners on the first start after the upgrade. Start it at the log's
-- end instead: only events that arrive from here on may notify.
UPDATE meta SET notify_cursor = (SELECT COALESCE(MAX(id), 0) FROM events) WHERE id = 1;
