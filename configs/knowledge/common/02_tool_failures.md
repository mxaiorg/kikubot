# Handling tool failures and your turn budget

You have a limited number of turns per task. Spending them re-trying calls that already failed is the most common way to run out of turns and deliver nothing. Avoid it:

- **Never repeat an identical failing tool call.** If a tool returned an error, do not call it again with the same arguments hoping for a different result — you will get the same error. Read the error message: it usually says exactly what was wrong (a bad flag, an inaccessible id, a missing argument). Change the arguments or the approach, or stop.
- **After two failures on the same sub-goal, change strategy.** If you can't list a folder, try a search instead. If you can't reach an item by id, try the shared-link or as_user path the tool documents. If a whole avenue is blocked, drop it and work with what you already have.
- **A 'not_found' / 'Ancestor folder is unaccessible' on a Box folder or file usually means it was reached via a shared link.** Pass the original `https://.../s/<token>` URL as `shared_link` to box__folder_list / box__file_download, or the owner's numeric user id as `as_user`. Don't retry the bare id.
- **Don't pass a shared-link URL where an id is expected.** A `https://.../s/<token>` URL is not a folder_id or file_id. Resolve it with box__shared_item_get first, then use the returned numeric id (and pass the URL as the separate `shared_link` argument).

**Always deliver something.** If you've gathered enough to produce a useful draft or partial answer, produce it — even if one source was unreachable. Returning a partial result with a short note about what you couldn't get ("I couldn't access the shared Box folder, so the sample is drawn from search results") is far more useful to the requester than burning your remaining turns chasing the last gap and timing out with nothing to show.
