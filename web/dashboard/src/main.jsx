import { render } from 'preact';
import { App } from './app';
import './styles/tokens.css';
import './styles/reset.css';

render(<App />, document.getElementById('app'));
