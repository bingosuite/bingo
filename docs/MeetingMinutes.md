# Existing Solutions Analysis

## Meeting 1: January 19th, 2025

- We met with Robert Laganière. He agreed to be our client for this project. He is a professor who teaches programming paradigms at the University of Ottawa.
- He is an expert at Go and has a lot of experience working with idiomatic patterns in go, especially in concurrent programming.
- We pitched our ideas and trajectory for the project. He seems onboard and is excited to work with us. We agreed to have bi-weekly meetings to discuss our progress and get feedback from him.
- We discussed the existing solutions for Go concurrency debugging. Robert mentioned that while there are tools like `go tool trace` and delve, they lack interactivity and a concurrency-focused approach.
- We brainstormed features that would be beneficial for our tool, such as visualizing goroutine interactions, detecting common concurrency bugs, and providing an intuitive UI for developers.

## Meeting 2: February 9th, 2025

- Second meeting with Robert Laganière. We presented our plan for the architecture of our debugger and did some system design together on a collaborative whiteboard. We used to opportunity to ask questions about idiomatic patterns in go, such as how to handle errors and how to structure the codebase for a project like this. We agreed to have set bi-weekly goals for the project to keep us on track and ensure we are making progress towards our milestones. Our goal for next meeting is to have a working prototype debugger that uses the websocket pipe to communicate with a client.

## Meeting 3: February 23rd, 2025

- Third meeting with Robert Lagnière. We presented the prototype on a basic go program and discussed issues we faced related to os thread swaping and goroutines. We discussed performance influences from our architecture decisions, and everything seems to make sense and work well so far. For the next meeting, we will enhance the client experience and program tests, as well as make the debugger work on a more complex program.

## Meeting 4: March 9th, 2025

- Fourth meeting with Robert Laganière. We talked about multi-platform support and how we should make the platform specific code as thin as possible, and abstract the debugger properly. Our current approach does not do this, and we have agreed to begin working on a refactor and take a pause on feature development. Overall we are still on track, and refactoring the foundation now is essential to keep the project maintainable and scalable in the future.

## Meeting 5: March 23rd, 2025

- Fifth meeting with Robert Laganière. We're still in refactoring mode and feature dev is still paused. We decided to unify the hub and debugger in the backend to avoid using an internal protocol, which was a bad architectural decision. This new approach is much more efficient and easier to maintain, but we still have to implement it fully and refactor the existing channels. We also engineered the state machine for the client-server interaction and concluded the state should be entirely server driven, since we can have multiple clients connected to the same session. We also spent time writing unit tests to improve our coverage. Next steps: implement the refactor fully and close the first chapter of the project, which is the basic debugger implementation.
