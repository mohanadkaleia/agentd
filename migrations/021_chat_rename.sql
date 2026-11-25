-- The built-in assignment that answers the conversation was called the front desk.
UPDATE assignments SET builtin = 'chat' WHERE builtin = 'front-desk';
UPDATE assignments SET name = 'chat' WHERE builtin = 'chat' AND name = 'front-desk';
