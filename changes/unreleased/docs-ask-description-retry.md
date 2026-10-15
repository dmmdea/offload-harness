### Fixed - offload_ask's description names the cross-seat retry and its two result fields

- **The description is current.** It said a second attempt means changing the question; a failed local first attempt now already gets one cross-seat retry when no route was named, and an identical repeat returns that answer with its retry. The `Returns` line now lists `retried_on?` and `retry_note?`. The compact profile's keep and dropped lists carry the two reworded sentences.
