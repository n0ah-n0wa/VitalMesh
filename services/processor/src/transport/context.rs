//! The identifiers of the request being handled, carried in a task-local so
//! that any code running on the request's task (handlers, error responses)
//! can read them without threading them through every signature.

/// Identifiers of the request being handled.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RequestContext {
    pub request_id: String,
    pub correlation_id: String,
}

tokio::task_local! {
    static CONTEXT: RequestContext;
}

/// Runs `future` with `context` as the current request context.
pub(super) async fn scope<F: Future>(context: RequestContext, future: F) -> F::Output {
    CONTEXT.scope(context, future).await
}

/// The request id of the request being handled on this task, or an empty
/// string outside a request.
pub fn current_request_id() -> String {
    CONTEXT
        .try_with(|ctx| ctx.request_id.clone())
        .unwrap_or_default()
}

/// The identifiers of the request being handled on this task.
pub fn current_context() -> Option<RequestContext> {
    CONTEXT.try_with(Clone::clone).ok()
}
